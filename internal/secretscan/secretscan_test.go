package secretscan

import "testing"

func TestScanFlagsSecrets(t *testing.T) {
	cases := []string{
		"key is sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWx",
		"token ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"-----BEGIN OPENSSH PRIVATE KEY-----",
		"PGPASSWORD=hunter2hunter2",
		"aws AKIA" + "ABCDEFGHIJKLMNOP", // split so the fixture is not a key-shaped literal
		"blob Zx8kQ2pL9vR4tY7uW1" + "eS3dF6gH0jK5mN8bV2cX4z",
	}
	for _, c := range cases {
		if Scan(c) == nil {
			t.Errorf("expected finding for %q", c)
		}
	}
}

func TestScanAllowsEvidence(t *testing.T) {
	cases := []string{
		"pushed 4f2a9c1e7b3d5a6f8e0c2b4d6f8a0c2e4b6d8f0a to origin",
		"see docs/ai/handover.md and internal/state/store.go",
		"session 3f1c2b9e-1d2c-4b5a-9e8f-0a1b2c3d4e5f",
		"MR !42 merged; branch fix/login-bug",
		"secret ref dev-db: read-only use; never print",
	}
	for _, c := range cases {
		if f := Scan(c); f != nil {
			t.Errorf("unexpected finding %q for %q", f.Kind, c)
		}
	}
}
