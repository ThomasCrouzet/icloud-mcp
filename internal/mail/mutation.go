package mail

import (
	"context"
	"errors"
	"strings"

	"github.com/ThomasCrouzet/icloud-mcp/internal/mail/imapadapter"
)

func (s *Client) SetMessageFlags(ctx context.Context, input SetFlagsInput) (SetFlagsResult, error) {
	if err := validateFlags(input); err != nil {
		return SetFlagsResult{}, err
	}
	const reconciliation = "Re-read the message flags before deciding whether to retry."
	value, err := s.mutate(ctx, reconciliation, func(session imapSession, phase *imapMutationPhase) (any, error) {
		selected, err := session.Select(input.Mailbox, false)
		if err != nil {
			return nil, err
		}
		if selected.UIDValidity != input.UIDValidity {
			return nil, newError(CodeConcurrentModification, "mailbox UIDVALIDITY changed; re-read before mutating")
		}
		if _, err := session.FetchFlags(input.UID); err != nil {
			return nil, err
		}
		caps := session.Capabilities()
		if caps.CondStore {
			if input.ExpectedModSeq == 0 {
				return nil, validationError("expected_modseq is required when CONDSTORE is available")
			}
			if !session.SupportsModifiedDetection() {
				return nil, newError(CodeProtocolError, "conditional flag mutation is unavailable because MODIFIED cannot be safely detected")
			}
			return nil, newError(CodeProtocolError, "conditional flag mutation is not implemented by this adapter")
		}
		flags := protocolFlags(input.Flags)
		phase.dispatched = true
		if err := session.StoreDelta(input.UID, input.Operation == FlagOperationAdd, flags); err != nil {
			return nil, mapIMAPError(err, ctx, true, reconciliation)
		}
		result := SetFlagsResult{
			Mailbox: input.Mailbox, UIDValidity: input.UIDValidity, UID: input.UID,
			ConditionalUpdate: false,
		}
		updated, err := session.FetchFlags(input.UID)
		if err != nil {
			result.ResultIncomplete = true
			return result, nil
		}
		result.Flags = publicFlags(updated.Flags)
		result.ModSeq = updated.ModSeq
		return result, nil
	})
	if err != nil {
		return SetFlagsResult{}, err
	}
	return value.(SetFlagsResult), nil
}

func protocolFlags(flags []MessageFlag) []string {
	out := make([]string, len(flags))
	for i, flag := range flags {
		switch flag {
		case FlagSeen:
			out[i] = "\\Seen"
		case FlagFlagged:
			out[i] = "\\Flagged"
		case FlagAnswered:
			out[i] = "\\Answered"
		}
	}
	return out
}

func (s *Client) MoveMessage(ctx context.Context, input MoveInput) (MoveResult, error) {
	if err := validateIdentity(input.Mailbox, input.UIDValidity, input.UID); err != nil {
		return MoveResult{}, err
	}
	if err := validateMailbox(input.Destination); err != nil {
		return MoveResult{}, err
	}
	if input.Destination == input.Mailbox {
		return MoveResult{}, validationError("destination must differ from the source mailbox")
	}
	const reconciliation = "Check both source and destination before deciding whether to move again."
	value, err := s.mutate(ctx, reconciliation, func(session imapSession, phase *imapMutationPhase) (any, error) {
		mailboxes, err := boundedMailboxList(session)
		if err != nil {
			return nil, err
		}
		if !selectableMailbox(mailboxes, input.Destination) {
			return nil, newError(CodeNotFound, "destination mailbox was not returned as selectable by LIST")
		}
		return moveWithSession(ctx, session, phase, input.Mailbox, input.UIDValidity, input.UID, input.Destination)
	})
	if err != nil {
		return MoveResult{}, err
	}
	return value.(MoveResult), nil
}

func (s *Client) TrashMessage(ctx context.Context, input TrashInput) (MoveResult, error) {
	if err := validateIdentity(input.Mailbox, input.UIDValidity, input.UID); err != nil {
		return MoveResult{}, err
	}
	const reconciliation = "Check both source and destination before deciding whether to move again."
	value, err := s.mutate(ctx, reconciliation, func(session imapSession, phase *imapMutationPhase) (any, error) {
		mailboxes, err := boundedMailboxList(session)
		if err != nil {
			return nil, err
		}
		var trash []string
		for _, mailbox := range mailboxes {
			if !isSelectable(mailbox) {
				continue
			}
			for _, attribute := range mailbox.Attributes {
				if strings.EqualFold(attribute, "\\Trash") {
					trash = append(trash, mailbox.Name)
					break
				}
			}
		}
		if len(trash) != 1 {
			return nil, newError(CodeProtocolError, "exactly one selectable SPECIAL-USE Trash mailbox is required")
		}
		if trash[0] == input.Mailbox {
			return nil, newError(CodeConflict, "message is already in the Trash mailbox")
		}
		return moveWithSession(ctx, session, phase, input.Mailbox, input.UIDValidity, input.UID, trash[0])
	})
	if err != nil {
		// mutate already maps with phase.dispatched; do not reclassify.
		return MoveResult{}, err
	}
	return value.(MoveResult), nil
}

func boundedMailboxList(session imapSession) ([]imapadapter.Mailbox, error) {
	mailboxes, err := session.List()
	if err != nil {
		return nil, err
	}
	if len(mailboxes) > MaxMailboxes {
		return nil, &imapadapter.Error{Kind: imapadapter.ErrorTooLarge}
	}
	return mailboxes, nil
}

func selectableMailbox(mailboxes []imapadapter.Mailbox, name string) bool {
	matches := 0
	for _, mailbox := range mailboxes {
		if mailbox.Name == name && isSelectable(mailbox) {
			matches++
		}
	}
	return matches == 1
}

func isSelectable(mailbox imapadapter.Mailbox) bool {
	for _, attribute := range mailbox.Attributes {
		if strings.EqualFold(attribute, "\\Noselect") || strings.EqualFold(attribute, "\\NonExistent") {
			return false
		}
	}
	return true
}

func moveWithSession(ctx context.Context, session imapSession, phase *imapMutationPhase, source string, uidValidity, uid uint32, destination string) (MoveResult, error) {
	selected, err := session.Select(source, false)
	if err != nil {
		return MoveResult{}, err
	}
	if selected.UIDValidity != uidValidity {
		return MoveResult{}, newError(CodeConcurrentModification, "mailbox UIDVALIDITY changed; re-read before moving")
	}
	if _, err := session.FetchFlags(uid); err != nil {
		return MoveResult{}, err
	}
	caps := session.Capabilities()
	result := MoveResult{Mailbox: source, UIDValidity: uidValidity, UID: uid, Destination: destination}
	if caps.Move {
		phase.dispatched = true
		data, err := session.NativeMove(uid, destination)
		if err != nil {
			return MoveResult{}, mapIMAPError(err, ctx, true, "Check both source and destination before deciding whether to move again.")
		}
		result.Method = "move"
		if (data.UIDValidity == 0) != (data.DestinationUID == 0) {
			return MoveResult{}, &Error{Code: CodeOutcomeUnknown, Message: "mail move succeeded with unusable COPYUID metadata", Reconciliation: "Check both source and destination before deciding whether to move again."}
		}
		result.DestinationUIDValidity = data.UIDValidity
		result.DestinationUID = data.DestinationUID
		return result, nil
	}
	if !caps.UIDPlus {
		return MoveResult{}, newError(CodeProtocolError, "server supports neither native MOVE nor the safe UIDPLUS fallback")
	}
	phase.dispatched = true
	data, err := session.Copy(uid, destination)
	if err != nil {
		return MoveResult{}, mapIMAPError(err, ctx, true, "Check the destination before deciding whether to move again.")
	}
	if data.UIDValidity == 0 || data.DestinationUID == 0 {
		return MoveResult{}, &Error{Code: CodeOutcomeUnknown, Message: "mail copy succeeded with unusable COPYUID metadata", Reconciliation: "A destination copy may exist. Check the destination before deciding whether to move again."}
	}
	if err := session.AddDeleted(uid); err != nil {
		return MoveResult{}, moveStepError(err, "copied_not_marked_deleted", "A destination copy may exist while the source remains. Check both mailboxes.")
	}
	if err := session.UIDExpunge(uid); err != nil {
		return MoveResult{}, moveStepError(err, "copied_marked_deleted_not_expunged", "A destination copy exists and the source may remain marked Deleted. Check both mailboxes.")
	}
	result.Method = "uidplus_fallback"
	result.DestinationUIDValidity = data.UIDValidity
	result.DestinationUID = data.DestinationUID
	return result, nil
}

func moveStepError(err error, category, reconciliation string) error {
	var protocolErr *imapadapter.Error
	if errors.As(err, &protocolErr) && protocolErr.Ambiguous {
		return &Error{Code: CodeOutcomeUnknown, Message: "mail move outcome is unknown after " + category, Reconciliation: reconciliation}
	}
	return &Error{Code: CodePartialFailure, Message: "mail move stopped after " + category, Reconciliation: reconciliation}
}


