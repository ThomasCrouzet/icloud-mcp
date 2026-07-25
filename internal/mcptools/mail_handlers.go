package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	stdmail "net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

type mailErrorPayload struct {
	Code           string                       `json:"code"`
	Message        string                       `json:"message"`
	Retryable      bool                         `json:"retryable,omitempty"`
	RetryAfter     int                          `json:"retry_after_seconds,omitempty"`
	Reconciliation string                       `json:"reconciliation,omitempty"`
	Status         maildomain.SendStatus        `json:"status,omitempty"`
	MessageID      string                       `json:"messageId,omitempty"`
	Recipients     []maildomain.RecipientStatus `json:"recipients,omitempty"`
}

func listMailboxesHandler(deps MailDeps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if _, err := mailArguments(req, nil); err != nil {
			return mailValidationResult(deps, err), nil
		}
		result, err := deps.Service.ListMailboxes(ctx)
		if err != nil {
			return mailServiceErrorResult(deps, "listing mailboxes", err), nil
		}
		if result.Mailboxes == nil {
			result.Mailboxes = []maildomain.Mailbox{}
		}
		return writeJSON(mailRedactor(deps), result), nil
	}
}

func searchMessagesHandler(deps MailDeps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := mailArguments(req, mailKeySet(
			"mailbox", "query", "from", "to", "subject", "since", "before",
			"unseen_only", "flagged_only", "before_uid", "uid_validity", "limit",
		))
		if err != nil {
			return mailValidationResult(deps, err), nil
		}
		input, err := mailSearchInput(args)
		if err != nil {
			return mailValidationResult(deps, err), nil
		}
		result, err := deps.Service.SearchMessages(ctx, input)
		if err != nil {
			return mailServiceErrorResult(deps, "searching messages", err), nil
		}
		if result.Messages == nil {
			result.Messages = []maildomain.MessageSummary{}
		}
		return writeJSON(mailRedactor(deps), result), nil
	}
}

func getMessageHandler(deps MailDeps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := mailArguments(req, mailKeySet("mailbox", "uid_validity", "uid", "max_body_bytes"))
		if err != nil {
			return mailValidationResult(deps, err), nil
		}
		identity, err := mailParseIdentity(args)
		if err != nil {
			return mailValidationResult(deps, err), nil
		}
		maxBody := uint64(maildomain.DefaultBodyBytes)
		if value, present, parseErr := mailOptionalUnsigned(args, "max_body_bytes", 32); parseErr != nil {
			return mailValidationResult(deps, parseErr), nil
		} else if present {
			maxBody = value
		}
		if maxBody < 1 || maxBody > maildomain.MaxBodyBytes {
			return mailValidationResult(deps, fmt.Errorf("max_body_bytes must be between 1 and %d", maildomain.MaxBodyBytes)), nil
		}
		result, err := deps.Service.GetMessage(ctx, maildomain.GetMessageInput{
			Mailbox: identity.Mailbox, UIDValidity: identity.UIDValidity, UID: identity.UID,
			MaxBodyBytes: int(maxBody),
		})
		if err != nil {
			return mailServiceErrorResult(deps, "getting message", err), nil
		}
		return writeJSON(mailRedactor(deps), result), nil
	}
}

func setMessageFlagsHandler(deps MailDeps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		const tool = "set_message_flags"
		args, err := mailArguments(req, mailKeySet("mailbox", "uid_validity", "uid", "operation", "flags", "expected_modseq"))
		if err != nil {
			mailAudit(deps, tool, "message", "invalid", "denied")
			return mailValidationResult(deps, err), nil
		}
		identity, err := mailParseIdentity(args)
		if err != nil {
			mailAudit(deps, tool, "message", mailAuditResource(args), "denied")
			return mailValidationResult(deps, err), nil
		}
		resource := mailIdentityResource(identity)
		operation, err := mailRequiredString(args, "operation")
		if err != nil || operation != string(maildomain.FlagOperationAdd) && operation != string(maildomain.FlagOperationRemove) {
			if err == nil {
				err = fmt.Errorf("operation must be add or remove")
			}
			mailAudit(deps, tool, "message", resource, "denied")
			return mailValidationResult(deps, err), nil
		}
		flagValues, err := mailStringArray(args, "flags", true)
		if err != nil {
			mailAudit(deps, tool, "message", resource, "denied")
			return mailValidationResult(deps, err), nil
		}
		flags, err := mailFlags(flagValues)
		if err != nil {
			mailAudit(deps, tool, "message", resource, "denied")
			return mailValidationResult(deps, err), nil
		}
		expectedModSeq, present, err := mailOptionalUnsigned(args, "expected_modseq", 64)
		if err != nil || present && expectedModSeq == 0 {
			if err == nil {
				err = fmt.Errorf("expected_modseq must be non-zero when provided")
			}
			mailAudit(deps, tool, "message", resource, "denied")
			return mailValidationResult(deps, err), nil
		}
		result, err := deps.Service.SetMessageFlags(ctx, maildomain.SetFlagsInput{
			Mailbox: identity.Mailbox, UIDValidity: identity.UIDValidity, UID: identity.UID,
			Operation: maildomain.FlagOperation(operation), Flags: flags, ExpectedModSeq: expectedModSeq,
		})
		if err != nil {
			mailAudit(deps, tool, "message", resource, mailAuditErrorStatus(err))
			return mailServiceErrorResult(deps, "setting message flags", err), nil
		}
		mailAudit(deps, tool, "message", resource, "success")
		return writeJSON(mailRedactor(deps), result), nil
	}
}

func moveMessageHandler(deps MailDeps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		const tool = "move_message"
		args, err := mailArguments(req, mailKeySet("mailbox", "uid_validity", "uid", "destination"))
		if err != nil {
			mailAudit(deps, tool, "message", "invalid", "denied")
			return mailValidationResult(deps, err), nil
		}
		identity, err := mailParseIdentity(args)
		if err != nil {
			mailAudit(deps, tool, "message", mailAuditResource(args), "denied")
			return mailValidationResult(deps, err), nil
		}
		resource := mailIdentityResource(identity)
		destination, err := mailRequiredMailbox(args, "destination")
		if err != nil {
			mailAudit(deps, tool, "message", resource, "denied")
			return mailValidationResult(deps, err), nil
		}
		if destination == identity.Mailbox {
			mailAudit(deps, tool, "message", resource, "denied")
			return mailValidationResult(deps, fmt.Errorf("destination must differ from mailbox")), nil
		}
		result, err := deps.Service.MoveMessage(ctx, maildomain.MoveInput{
			Mailbox: identity.Mailbox, UIDValidity: identity.UIDValidity, UID: identity.UID, Destination: destination,
		})
		if err != nil {
			mailAudit(deps, tool, "message", resource, mailAuditErrorStatus(err))
			return mailServiceErrorResult(deps, "moving message", err), nil
		}
		mailAudit(deps, tool, "message", resource, "success")
		return writeJSON(mailRedactor(deps), result), nil
	}
}

func trashMessageHandler(deps MailDeps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		const tool = "trash_message"
		args, err := mailArguments(req, mailKeySet("mailbox", "uid_validity", "uid"))
		if err != nil {
			mailAudit(deps, tool, "message", "invalid", "denied")
			return mailValidationResult(deps, err), nil
		}
		identity, err := mailParseIdentity(args)
		if err != nil {
			mailAudit(deps, tool, "message", mailAuditResource(args), "denied")
			return mailValidationResult(deps, err), nil
		}
		resource := mailIdentityResource(identity)
		result, err := deps.Service.TrashMessage(ctx, maildomain.TrashInput{
			Mailbox: identity.Mailbox, UIDValidity: identity.UIDValidity, UID: identity.UID,
		})
		if err != nil {
			mailAudit(deps, tool, "message", resource, mailAuditErrorStatus(err))
			return mailServiceErrorResult(deps, "trashing message", err), nil
		}
		mailAudit(deps, tool, "message", resource, "success")
		return writeJSON(mailRedactor(deps), result), nil
	}
}

func sendMessageHandler(deps MailDeps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		const tool = "send_message"
		args, err := mailArguments(req, mailKeySet("to", "cc", "bcc", "subject", "body"))
		if err != nil {
			mailAudit(deps, tool, "submission", "invalid", "denied")
			return mailSendValidationResult(deps, err), nil
		}
		input, err := mailSendInput(args)
		if err != nil {
			mailAudit(deps, tool, "submission", "invalid", "denied")
			return mailSendValidationResult(deps, err), nil
		}
		result, err := deps.Service.SendMessage(ctx, input)
		resource := result.MessageID
		if resource == "" {
			resource = "submission"
		}
		if err != nil {
			mailAudit(deps, tool, "submission", resource, mailAuditErrorStatus(err))
			return mailSendErrorResult(deps, "sending message", result, err), nil
		}
		status := "error"
		switch result.Status {
		case maildomain.SendAccepted:
			status = "success"
		case maildomain.SendOutcomeUnknown:
			status = "outcome_unknown"
		}
		mailAudit(deps, tool, "submission", resource, status)
		return writeJSON(mailRedactor(deps), result), nil
	}
}

func mailServiceErrorResult(deps MailDeps, operation string, err error) *mcp.CallToolResult {
	payload := mailErrorPayload{Code: string(maildomain.CodeInternalError), Message: operation + ": mail operation failed"}
	if public := maildomain.AsError(err); public != nil {
		payload.Code = string(public.Code)
		payload.Message = operation + ": " + public.Message
		payload.Retryable = public.Retryable
		payload.Reconciliation = public.Reconciliation
		payload.RetryAfter = mailRetryAfterSeconds(public)
		if public.Code == maildomain.CodeOutcomeUnknown {
			payload.Status = maildomain.SendOutcomeUnknown
		}
	}
	result := writeJSON(mailRedactor(deps), payload)
	result.IsError = true
	return result
}

func mailSendErrorResult(deps MailDeps, operation string, send maildomain.SendResult, err error) *mcp.CallToolResult {
	payload := mailErrorPayload{
		Code:       string(maildomain.CodeInternalError),
		Message:    operation + ": mail operation failed",
		Status:     send.Status,
		MessageID:  send.MessageID,
		Recipients: append([]maildomain.RecipientStatus(nil), send.Recipients...),
	}
	if public := maildomain.AsError(err); public != nil {
		payload.Code = string(public.Code)
		payload.Message = operation + ": " + public.Message
		payload.Retryable = public.Retryable
		payload.Reconciliation = public.Reconciliation
		payload.RetryAfter = mailRetryAfterSeconds(public)
		if public.Code == maildomain.CodeOutcomeUnknown {
			payload.Status = maildomain.SendOutcomeUnknown
		} else if payload.Status == "" {
			payload.Status = maildomain.SendRejected
		}
	}
	result := writeJSON(mailRedactor(deps), payload)
	result.IsError = true
	return result
}

func mailRetryAfterSeconds(err *maildomain.Error) int {
	if err == nil || err.RetryAfter <= 0 {
		return 0
	}
	sec := int(err.RetryAfter.Seconds())
	if sec < 1 {
		return 1
	}
	if sec > 60 {
		return 60
	}
	return sec
}

func mailValidationResult(deps MailDeps, err error) *mcp.CallToolResult {
	payload := mailErrorPayload{Code: string(maildomain.CodeValidation), Message: "validation: " + err.Error()}
	result := writeJSON(mailRedactor(deps), payload)
	result.IsError = true
	return result
}

func mailSendValidationResult(deps MailDeps, err error) *mcp.CallToolResult {
	payload := mailErrorPayload{
		Code:    string(maildomain.CodeValidation),
		Message: "validation: " + err.Error(),
		Status:  maildomain.SendRejected,
	}
	result := writeJSON(mailRedactor(deps), payload)
	result.IsError = true
	return result
}

func mailRedactor(deps MailDeps) *security.Redactor {
	if deps.Redactor == nil {
		// RegisterMail panics on nil; keep a hard fail if a handler is miswired.
		panic("mcptools: Mail handler missing redactor")
	}
	return deps.Redactor
}

func mailAudit(deps MailDeps, tool, resourceType, resource, status string) {
	if deps.Audit != nil {
		deps.Audit.LogDomainMutation(tool, "mail", resourceType, resource, status)
	}
}

func mailAuditErrorStatus(err error) string {
	if public := maildomain.AsError(err); public != nil {
		switch public.Code {
		case maildomain.CodeOutcomeUnknown:
			return "outcome_unknown"
		case maildomain.CodeValidation:
			return "denied"
		}
	}
	return "error"
}

type mailIdentity struct {
	Mailbox     string
	UIDValidity uint32
	UID         uint32
}

func mailParseIdentity(args map[string]any) (mailIdentity, error) {
	mailbox, err := mailRequiredMailbox(args, "mailbox")
	if err != nil {
		return mailIdentity{}, err
	}
	uidValidity, err := mailRequiredUint32(args, "uid_validity")
	if err != nil {
		return mailIdentity{}, err
	}
	uid, err := mailRequiredUint32(args, "uid")
	if err != nil {
		return mailIdentity{}, err
	}
	return mailIdentity{Mailbox: mailbox, UIDValidity: uidValidity, UID: uid}, nil
}

func mailIdentityResource(identity mailIdentity) string {
	return identity.Mailbox + "\x00" + strconv.FormatUint(uint64(identity.UIDValidity), 10) + "\x00" + strconv.FormatUint(uint64(identity.UID), 10)
}

func mailAuditResource(args map[string]any) string {
	mailbox, _ := args["mailbox"].(string)
	return mailbox + "\x00invalid"
}

func mailSearchInput(args map[string]any) (maildomain.SearchInput, error) {
	mailbox, err := mailRequiredMailbox(args, "mailbox")
	if err != nil {
		return maildomain.SearchInput{}, err
	}
	input := maildomain.SearchInput{Mailbox: mailbox, Limit: 20}
	for _, field := range []struct {
		name   string
		target *string
	}{
		{"query", &input.Query}, {"from", &input.From}, {"to", &input.To}, {"subject", &input.Subject},
	} {
		value, present, parseErr := mailOptionalString(args, field.name)
		if parseErr != nil {
			return maildomain.SearchInput{}, parseErr
		}
		if present {
			if err := mailValidateSearchValue(field.name, value); err != nil {
				return maildomain.SearchInput{}, err
			}
			*field.target = value
		}
	}
	if input.Since, _, err = mailOptionalDate(args, "since"); err != nil {
		return maildomain.SearchInput{}, err
	}
	if input.Before, _, err = mailOptionalDate(args, "before"); err != nil {
		return maildomain.SearchInput{}, err
	}
	if !input.Since.IsZero() && !input.Before.IsZero() && !input.Before.After(input.Since) {
		return maildomain.SearchInput{}, fmt.Errorf("before must be after since")
	}
	if input.UnseenOnly, _, err = mailOptionalBool(args, "unseen_only"); err != nil {
		return maildomain.SearchInput{}, err
	}
	if input.FlaggedOnly, _, err = mailOptionalBool(args, "flagged_only"); err != nil {
		return maildomain.SearchInput{}, err
	}
	beforeUID, beforePresent, err := mailOptionalUnsigned(args, "before_uid", 32)
	if err != nil {
		return maildomain.SearchInput{}, err
	}
	uidValidity, uidValidityPresent, err := mailOptionalUnsigned(args, "uid_validity", 32)
	if err != nil {
		return maildomain.SearchInput{}, err
	}
	if beforePresent && beforeUID == 0 {
		return maildomain.SearchInput{}, fmt.Errorf("before_uid must be non-zero when provided")
	}
	if uidValidityPresent && uidValidity == 0 {
		return maildomain.SearchInput{}, fmt.Errorf("uid_validity must be non-zero when provided")
	}
	if beforePresent && !uidValidityPresent {
		return maildomain.SearchInput{}, fmt.Errorf("uid_validity is required with before_uid")
	}
	input.BeforeUID = uint32(beforeUID)
	input.UIDValidity = uint32(uidValidity)
	if limit, present, parseErr := mailOptionalUnsigned(args, "limit", 32); parseErr != nil {
		return maildomain.SearchInput{}, parseErr
	} else if present {
		if limit < 1 || limit > maildomain.MaxSearchResults {
			return maildomain.SearchInput{}, fmt.Errorf("limit must be between 1 and %d", maildomain.MaxSearchResults)
		}
		input.Limit = int(limit)
	}
	return input, nil
}

func mailFlags(values []string) ([]maildomain.MessageFlag, error) {
	if len(values) < 1 || len(values) > 3 {
		return nil, fmt.Errorf("flags must contain between 1 and 3 values")
	}
	flags := make([]maildomain.MessageFlag, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		switch maildomain.MessageFlag(value) {
		case maildomain.FlagSeen, maildomain.FlagFlagged, maildomain.FlagAnswered:
		default:
			return nil, fmt.Errorf("flags may contain only seen, flagged, or answered")
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, fmt.Errorf("flags must not contain duplicate values")
		}
		seen[value] = struct{}{}
		flags = append(flags, maildomain.MessageFlag(value))
	}
	return flags, nil
}

func mailSendInput(args map[string]any) (maildomain.SendInput, error) {
	var input maildomain.SendInput
	var err error
	for _, field := range []struct {
		name   string
		target *[]string
	}{{"to", &input.To}, {"cc", &input.Cc}, {"bcc", &input.Bcc}} {
		values, parseErr := mailStringArray(args, field.name, false)
		if parseErr != nil {
			return maildomain.SendInput{}, parseErr
		}
		if len(values) > maildomain.MaxRecipients {
			return maildomain.SendInput{}, fmt.Errorf("%s must contain at most %d recipients", field.name, maildomain.MaxRecipients)
		}
		*field.target = values
	}
	if len(input.To)+len(input.Cc)+len(input.Bcc) == 0 {
		return maildomain.SendInput{}, fmt.Errorf("at least one recipient is required across to, cc, and bcc")
	}
	if len(input.To)+len(input.Cc)+len(input.Bcc) > maildomain.MaxRecipients {
		return maildomain.SendInput{}, fmt.Errorf("recipient count must not exceed %d", maildomain.MaxRecipients)
	}
	seen := make(map[string]struct{}, len(input.To)+len(input.Cc)+len(input.Bcc))
	for _, values := range [][]string{input.To, input.Cc, input.Bcc} {
		for _, address := range values {
			if err := mailValidateAddress(address); err != nil {
				return maildomain.SendInput{}, err
			}
			key := mailASCIILower(address)
			if _, duplicate := seen[key]; duplicate {
				return maildomain.SendInput{}, fmt.Errorf("recipient addresses must be unique")
			}
			seen[key] = struct{}{}
		}
	}
	input.Subject, _, err = mailOptionalString(args, "subject")
	if err != nil {
		return maildomain.SendInput{}, err
	}
	if len(input.Subject) > maildomain.MaxSubjectBytes || !utf8.ValidString(input.Subject) || mailHasDisallowedControl(input.Subject, false) {
		return maildomain.SendInput{}, fmt.Errorf("subject contains invalid or excessive data")
	}
	input.Body, err = mailRequiredString(args, "body")
	if err != nil {
		return maildomain.SendInput{}, err
	}
	if len(input.Body) > maildomain.MaxSendBodyBytes || !utf8.ValidString(input.Body) || strings.ContainsRune(input.Body, '\x00') {
		return maildomain.SendInput{}, fmt.Errorf("body contains invalid or excessive data")
	}
	return input, nil
}

func mailValidateAddress(value string) error {
	if value == "" || len(value) > 320 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n") || !utf8.ValidString(value) {
		return fmt.Errorf("recipients must be valid plain addr-specs")
	}
	parsed, err := stdmail.ParseAddress(value)
	if err != nil || parsed.Name != "" || parsed.Address != value || !strings.Contains(value, "@") {
		return fmt.Errorf("recipients must be valid plain addr-specs")
	}
	return nil
}

func mailASCIILower(value string) string {
	buffer := []byte(value)
	for index := range buffer {
		if buffer[index] >= 'A' && buffer[index] <= 'Z' {
			buffer[index] += 'a' - 'A'
		}
	}
	return string(buffer)
}

func mailRequiredMailbox(args map[string]any, name string) (string, error) {
	value, err := mailRequiredString(args, name)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", fmt.Errorf("%s cannot be empty", name)
	}
	if len(value) > maildomain.MaxMetadataString || !utf8.ValidString(value) || mailHasDisallowedControl(value, true) {
		return "", fmt.Errorf("%s contains invalid or excessive data", name)
	}
	return value, nil
}

func mailValidateSearchValue(name, value string) error {
	if len(value) > maildomain.MaxSearchValue || !utf8.ValidString(value) || mailHasDisallowedControl(value, false) {
		return fmt.Errorf("%s contains invalid or excessive data", name)
	}
	return nil
}

func mailHasDisallowedControl(value string, allowTab bool) bool {
	for _, r := range value {
		if r == 0x7f || r < 0x20 && (r != '\t' || !allowTab) {
			return true
		}
	}
	return false
}

func mailArguments(req mcp.CallToolRequest, allowed map[string]struct{}) (map[string]any, error) {
	raw := req.GetRawArguments()
	var args map[string]any
	switch value := raw.(type) {
	case json.RawMessage:
		if err := mailDecodeJSONArguments(value, &args); err != nil {
			return nil, err
		}
	case []byte:
		if err := mailDecodeJSONArguments(value, &args); err != nil {
			return nil, err
		}
	case map[string]any:
		args = value
	case nil:
		args = map[string]any{}
	default:
		return nil, fmt.Errorf("arguments must be a JSON object")
	}
	if args == nil {
		return nil, fmt.Errorf("arguments must be a JSON object")
	}
	for key := range args {
		if _, ok := allowed[key]; !ok {
			return nil, fmt.Errorf("unsupported argument")
		}
	}
	return args, nil
}

func mailDecodeJSONArguments(data []byte, target *map[string]any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("arguments must be a valid JSON object")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("arguments must contain one JSON object")
	}
	return nil
}

func mailKeySet(keys ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		set[key] = struct{}{}
	}
	return set
}

func mailRequiredString(args map[string]any, name string) (string, error) {
	value, ok := args[name]
	if !ok {
		return "", fmt.Errorf("%s is required", name)
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", name)
	}
	return text, nil
}

func mailOptionalString(args map[string]any, name string) (string, bool, error) {
	value, ok := args[name]
	if !ok {
		return "", false, nil
	}
	text, ok := value.(string)
	if !ok {
		return "", false, fmt.Errorf("%s must be a string", name)
	}
	return text, true, nil
}

func mailOptionalBool(args map[string]any, name string) (bool, bool, error) {
	value, ok := args[name]
	if !ok {
		return false, false, nil
	}
	boolean, ok := value.(bool)
	if !ok {
		return false, false, fmt.Errorf("%s must be a boolean", name)
	}
	return boolean, true, nil
}

func mailRequiredUint32(args map[string]any, name string) (uint32, error) {
	value, present, err := mailOptionalUnsigned(args, name, 32)
	if err != nil {
		return 0, err
	}
	if !present {
		return 0, fmt.Errorf("%s is required", name)
	}
	if value == 0 {
		return 0, fmt.Errorf("%s must be non-zero", name)
	}
	return uint32(value), nil
}

func mailOptionalUnsigned(args map[string]any, name string, bits int) (uint64, bool, error) {
	raw, present := args[name]
	if !present {
		return 0, false, nil
	}
	value, err := mailUnsigned(raw, bits)
	if err != nil {
		return 0, false, fmt.Errorf("%s must be an unsigned %d-bit integer", name, bits)
	}
	return value, true, nil
}

func mailUnsigned(raw any, bits int) (uint64, error) {
	if number, ok := raw.(json.Number); ok {
		value, err := strconv.ParseUint(string(number), 10, bits)
		if err != nil {
			return 0, err
		}
		return value, nil
	}
	var value uint64
	switch number := raw.(type) {
	case uint:
		value = uint64(number)
	case uint8:
		value = uint64(number)
	case uint16:
		value = uint64(number)
	case uint32:
		value = uint64(number)
	case uint64:
		value = number
	case int:
		if number < 0 {
			return 0, fmt.Errorf("negative integer")
		}
		value = uint64(number)
	case int8:
		if number < 0 {
			return 0, fmt.Errorf("negative integer")
		}
		value = uint64(number)
	case int16:
		if number < 0 {
			return 0, fmt.Errorf("negative integer")
		}
		value = uint64(number)
	case int32:
		if number < 0 {
			return 0, fmt.Errorf("negative integer")
		}
		value = uint64(number)
	case int64:
		if number < 0 {
			return 0, fmt.Errorf("negative integer")
		}
		value = uint64(number)
	case float64:
		if math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || math.Trunc(number) != number || number > 1<<53 {
			return 0, fmt.Errorf("unsafe numeric value")
		}
		value = uint64(number)
	case float32:
		converted := float64(number)
		if math.IsNaN(converted) || math.IsInf(converted, 0) || converted < 0 || math.Trunc(converted) != converted || converted > 1<<53 {
			return 0, fmt.Errorf("unsafe numeric value")
		}
		value = uint64(converted)
	default:
		return 0, fmt.Errorf("not a number")
	}
	if bits == 32 && value > uint64(^uint32(0)) {
		return 0, fmt.Errorf("integer overflow")
	}
	return value, nil
}

func mailStringArray(args map[string]any, name string, required bool) ([]string, error) {
	raw, present := args[name]
	if !present {
		if required {
			return nil, fmt.Errorf("%s is required", name)
		}
		return nil, nil
	}
	switch values := raw.(type) {
	case []string:
		return append([]string(nil), values...), nil
	case []any:
		result := make([]string, len(values))
		for index, value := range values {
			text, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("%s item %d must be a string", name, index)
			}
			result[index] = text
		}
		return result, nil
	default:
		return nil, fmt.Errorf("%s must be an array of strings", name)
	}
}

func mailOptionalDate(args map[string]any, name string) (time.Time, bool, error) {
	value, present, err := mailOptionalString(args, name)
	if err != nil || !present {
		return time.Time{}, present, err
	}
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("%s must be a valid YYYY-MM-DD date", name)
	}
	return parsed, true, nil
}
