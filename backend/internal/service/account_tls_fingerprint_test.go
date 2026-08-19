package service

import "testing"

func TestIsTLSFingerprintEnabledAccountScope(t *testing.T) {
	tests := []struct {
		name     string
		platform string
		accType  string
		want     bool
	}{
		{name: "openai apikey", platform: PlatformOpenAI, accType: AccountTypeAPIKey, want: true},
		{name: "anthropic oauth", platform: PlatformAnthropic, accType: AccountTypeOAuth, want: true},
		{name: "anthropic setup token", platform: PlatformAnthropic, accType: AccountTypeSetupToken, want: true},
		{name: "openai oauth", platform: PlatformOpenAI, accType: AccountTypeOAuth, want: false},
		{name: "gemini apikey", platform: PlatformGemini, accType: AccountTypeAPIKey, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{
				Platform: tt.platform,
				Type:     tt.accType,
				Extra:    map[string]any{"enable_tls_fingerprint": true},
			}
			if got := account.IsTLSFingerprintEnabled(); got != tt.want {
				t.Fatalf("IsTLSFingerprintEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}
