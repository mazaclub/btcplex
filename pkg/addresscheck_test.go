package btcplex

import (
	"testing"
)

func TestValidA58(t *testing.T) {
	type AddrTest struct {
		Addr    string
		IsValid bool
	}
	addrTests := []AddrTest{
		// Blank hash160
		{"M7uAERuQW2AotfyLDyewFGcLUDtAYu9v5V", true},
		// Blank hash160, address version 51
		{"MXEmDYChDCdgi77RFPzFjPt86j97MwEZsu", false},
	}

	for _, addrTest := range addrTests {
		s := []byte(addrTest.Addr)
		ok, _ := ValidA58(s)
		if ok != addrTest.IsValid {
			t.Error("For validation of address", addrTest.Addr,
				"expected", addrTest.IsValid, "got", ok)
		}
	}
}
