package server

import (
	"testing"

	"nft-forward/internal/db"
	"nft-forward/internal/landing"
)

func TestClassifyExit(t *testing.T) {
	idx := landingIndex([]landing.Node{
		{Name: "HK-01", Protocol: "vless", Host: "1.2.3.4", Port: 443,
			URI: "vless://uuid@1.2.3.4:443?security=reality&sni=a.com#HK-01"},
	})

	t.Run("landing match yields relay uri with entry endpoint", func(t *testing.T) {
		it := ruleListItem{Rule: &db.Rule{ExitHost: "1.2.3.4", ExitPort: 443},
			Entry: "relay.example:10001", Exit: "1.2.3.4:443"}
		it.classifyExit(idx, true)
		if it.ExitKind != "landing" {
			t.Fatalf("kind = %q, want landing", it.ExitKind)
		}
		if it.LandingName != "HK-01" {
			t.Errorf("landing_name = %q", it.LandingName)
		}
		want := "vless://uuid@relay.example:10001?security=reality&sni=a.com#HK-01"
		if it.RelayURI != want {
			t.Errorf("relay_uri = %q, want %q", it.RelayURI, want)
		}
	})

	t.Run("admin list (withURI=false) marks kind but omits relay uri", func(t *testing.T) {
		it := ruleListItem{Rule: &db.Rule{ExitHost: "1.2.3.4", ExitPort: 443},
			Entry: "relay.example:10001", Exit: "1.2.3.4:443"}
		it.classifyExit(idx, false)
		if it.ExitKind != "landing" || it.RelayURI != "" {
			t.Fatalf("kind=%q relay=%q, want landing with empty relay", it.ExitKind, it.RelayURI)
		}
	})

	t.Run("custom exit with stored uri yields relay uri", func(t *testing.T) {
		it := ruleListItem{Rule: &db.Rule{ExitHost: "9.9.9.9", ExitPort: 8443,
			ExitURI: "trojan://pass@9.9.9.9:8443?sni=b.com#Custom"},
			Entry: "relay.example:20000", Exit: "9.9.9.9:8443"}
		it.classifyExit(idx, true)
		if it.ExitKind != "custom" {
			t.Fatalf("kind = %q, want custom", it.ExitKind)
		}
		want := "trojan://pass@relay.example:20000?sni=b.com#Custom"
		if it.RelayURI != want {
			t.Errorf("relay_uri = %q, want %q", it.RelayURI, want)
		}
	})

	t.Run("custom exit without uri has no relay uri", func(t *testing.T) {
		it := ruleListItem{Rule: &db.Rule{ExitHost: "9.9.9.9", ExitPort: 8443},
			Entry: "relay.example:20000", Exit: "9.9.9.9:8443"}
		it.classifyExit(idx, true)
		if it.ExitKind != "custom" || it.RelayURI != "" || it.LandingURI != "" {
			t.Fatalf("got kind=%q relay=%q landing=%q", it.ExitKind, it.RelayURI, it.LandingURI)
		}
	})

	t.Run("no entry yet skips relay uri", func(t *testing.T) {
		it := ruleListItem{Rule: &db.Rule{ExitHost: "1.2.3.4", ExitPort: 443},
			Entry: "—", Exit: "1.2.3.4:443"}
		it.classifyExit(idx, true)
		if it.ExitKind != "landing" || it.RelayURI != "" {
			t.Fatalf("kind=%q relay=%q, want landing with empty relay", it.ExitKind, it.RelayURI)
		}
	})
}
