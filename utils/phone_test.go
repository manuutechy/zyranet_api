package utils

import "testing"

func TestFormatPhone(t *testing.T) {
	cases := map[string]string{
		"0712345678":     "254712345678",
		"0112345678":     "254112345678",
		"712345678":      "254712345678",
		"112345678":      "254112345678",
		"254712345678":   "254712345678",
		"254254712345678": "254712345678",
		"2540712345678":  "254712345678",
		"+254712345678":  "254712345678",
		"0712 345 678":   "254712345678",
	}
	for input, want := range cases {
		if got := FormatPhone(input); got != want {
			t.Errorf("FormatPhone(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestFormatPhoneE164(t *testing.T) {
	if got := FormatPhoneE164("0712345678"); got != "+254712345678" {
		t.Errorf("FormatPhoneE164(\"0712345678\") = %q, want \"+254712345678\"", got)
	}
}
