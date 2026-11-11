package models

import "testing"

func TestCustomerNoFor(t *testing.T) {
	if got := CustomerNoFor(1); got != 10017 {
		t.Errorf("CustomerNoFor(1) = %d, want 10017", got)
	}
	for id := uint(1); id < 5000; id++ {
		no := CustomerNoFor(id)
		if got, ok := ParseCustomerNo(uintStr(no)); !ok || got != no {
			t.Fatalf("number of customer %d (%d) does not read back", id, no)
		}
	}
}

func uintStr(n uint) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

func TestParseCustomerNo_HowPeopleTypeIt(t *testing.T) {
	for _, in := range []string{"10017", " 10017 ", "#10017", "zyr10017", "ZYR#10017", "Z10017", "z 10 017", "zyr-10017", "Zyr #10017"} {
		if no, ok := ParseCustomerNo(in); !ok || no != 10017 {
			t.Errorf("ParseCustomerNo(%q) = %d,%v want 10017", in, no, ok)
		}
	}
	// Typos (wrong check digit, swapped digits), phones, junk: never a number.
	for _, in := range []string{"10018", "10071", "01017", "0712345678", "254712345678", "1001", "", "abc", "ZYR#254712345678", "10017x"} {
		if no, ok := ParseCustomerNo(in); ok {
			t.Errorf("ParseCustomerNo(%q) = %d, want rejected", in, no)
		}
	}
}
