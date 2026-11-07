package security

import "crypto/tls"

// NewIMAPFixtureDialer supplies synthetic trust roots only to protocol tests.
func NewIMAPFixtureDialer(dial DialContextFunc, config *tls.Config) DialContextFunc {
	return newIMAPDialer(dial, config)
}
