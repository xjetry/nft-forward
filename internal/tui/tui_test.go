package tui

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"nft-forward/internal/nft"
)

// stripANSI removes ANSI color/style escape sequences from s.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// fixedPortion returns the byte-prefix of s whose lipgloss display width equals
// exactly targetCells. It stops at the rune boundary where accumulated width
// first reaches targetCells. If s is narrower than targetCells, s is returned as-is.
func fixedPortion(s string, targetCells int) string {
	acc := 0
	for i, r := range s {
		rw := lipgloss.Width(string(r))
		if acc+rw > targetCells {
			return s[:i]
		}
		acc += rw
		if acc == targetCells {
			return s[:i+len(string(r))]
		}
	}
	return s
}

// TestRenderTableRow_ColumnAlignment verifies that header and data rows rendered
// through renderTableRow have identical fixed-column widths regardless of
// destination address length.
func TestRenderTableRow_ColumnAlignment(t *testing.T) {
	header := renderTableRow("协议", "监听", "→ 目标", "备注")
	headerPlain := stripANSI(header)

	rows := []struct {
		proto   string
		port    string
		dest    string
		comment string
	}{
		{"TCP", "8088", "→ 100.100.100.11:8088", "T"},
		{"TCP", "10010", "→ 127.0.0.1:10011", "local"},
		{"UDP", "53", "→ 8.8.8.8:53", "dns"},
		// Long destination that must be truncated.
		{"TCP", "9999", "→ very-long-hostname.example.com:12345", "overflow"},
	}

	expectedFixedCells := colProto + colSrcPort + colDest

	// The header's fixed portion must be exactly expectedFixedCells wide.
	hFixed := fixedPortion(headerPlain, expectedFixedCells)
	hFixedW := lipgloss.Width(hFixed)
	if hFixedW != expectedFixedCells {
		t.Fatalf("header fixed-column display width = %d, want %d (portion: %q)",
			hFixedW, expectedFixedCells, hFixed)
	}

	for _, row := range rows {
		line := renderTableRow(row.proto, row.port, row.dest, row.comment)
		linePlain := stripANSI(line)

		// 1. Every row must be at least as wide as the fixed columns.
		lineW := lipgloss.Width(linePlain)
		if lineW < expectedFixedCells {
			t.Errorf("row dest=%q: total width %d < fixed column total %d",
				row.dest, lineW, expectedFixedCells)
		}

		// 2. The fixed-column portion of every data row must be the same display
		//    width as the header, so the comment column starts at the same cell.
		rFixed := fixedPortion(linePlain, expectedFixedCells)
		rFixedW := lipgloss.Width(rFixed)
		if rFixedW != expectedFixedCells {
			t.Errorf("row dest=%q: fixed-column width = %d, want %d (portion: %q)",
				row.dest, rFixedW, expectedFixedCells, rFixed)
		}
	}
}

// TestRenderTableRow_TruncationEllipsis verifies that a destination string longer
// than colDest cells is truncated and the dest cell ends with "…".
func TestRenderTableRow_TruncationEllipsis(t *testing.T) {
	longDest := "→ " + strings.Repeat("x", 100)
	line := renderTableRow("TCP", "80", longDest, "")
	plain := stripANSI(line)

	// Skip the proto+srcPort prefix and examine the dest cell.
	afterProtoPort := fixedPortion(plain, colProto+colSrcPort)
	destAndRest := plain[len(afterProtoPort):]
	destCellStr := fixedPortion(destAndRest, colDest)

	if !strings.Contains(destCellStr, "…") {
		t.Errorf("expected ellipsis in truncated dest cell, got: %q", destCellStr)
	}

	// Also verify the dest cell is exactly colDest wide (lipgloss padding fills it).
	destCellW := lipgloss.Width(destCellStr)
	if destCellW != colDest {
		t.Errorf("dest cell width = %d, want %d", destCellW, colDest)
	}
}

// TestTruncateCell verifies CJK-aware truncation.
func TestTruncateCell(t *testing.T) {
	cases := []struct {
		input    string
		maxCells int
		truncate bool // whether we expect truncation
	}{
		{"hello", 10, false},
		{"hello world!", 8, true},
		// CJK: each char = 2 cells; "你好世界测试" = 12 cells; limit 6 → only 2 chars + "…"
		{"你好世界测试", 6, true},
	}
	for _, c := range cases {
		got := truncateCell(c.input, c.maxCells)
		runes := []rune(got)

		if !c.truncate {
			if got != c.input {
				t.Errorf("truncateCell(%q, %d) = %q, want no change", c.input, c.maxCells, got)
			}
		} else {
			// Truncation expected: last rune must be "…" and total width <= maxCells.
			if len(runes) == 0 || string(runes[len(runes)-1]) != "…" {
				t.Errorf("truncateCell(%q, %d) = %q, want ellipsis suffix", c.input, c.maxCells, got)
			}
			w := lipgloss.Width(got)
			if w > c.maxCells {
				t.Errorf("truncateCell(%q, %d) width %d > maxCells %d", c.input, c.maxCells, w, c.maxCells)
			}
		}
	}
}

// TestViewList_ColumnConsistency builds rows for mixed-length rules and
// checks that each data row has the same fixed-column display width as the header.
func TestViewList_ColumnConsistency(t *testing.T) {
	rules := []nft.Rule{
		{Proto: "tcp", SrcPort: 8088, DestIP: "100.100.100.11", DestPort: 8088, Comment: "T"},
		{Proto: "tcp", SrcPort: 10010, DestIP: "127.0.0.1", DestPort: 10011, Comment: "local"},
		{Proto: "udp", SrcPort: 53, DestHost: "dns.example.com", DestIP: "8.8.8.8", DestPort: 53, Comment: "dns-forward"},
	}

	header := renderTableRow("协议", "监听", "→ 目标", "备注")
	headerPlain := stripANSI(header)
	expectedFixed := colProto + colSrcPort + colDest

	hFixed := fixedPortion(headerPlain, expectedFixed)
	if lipgloss.Width(hFixed) != expectedFixed {
		t.Fatalf("header fixed width = %d, want %d", lipgloss.Width(hFixed), expectedFixed)
	}

	for _, r := range rules {
		target := r.DestIP
		if r.DestHost != "" {
			if r.DestIP != "" {
				target = fmt.Sprintf("%s (→ %s)", r.DestHost, r.DestIP)
			} else {
				target = r.DestHost
			}
		}
		destCell := fmt.Sprintf("→ %s:%d", target, r.DestPort)

		line := renderTableRow(
			strings.ToUpper(r.Proto),
			fmt.Sprintf("%d", r.SrcPort),
			destCell,
			r.Comment,
		)
		plain := stripANSI(line)
		rFixed := fixedPortion(plain, expectedFixed)
		rFixedW := lipgloss.Width(rFixed)
		if rFixedW != expectedFixed {
			t.Errorf("rule %s/%d dest=%q: fixed-column width = %d, want %d",
				r.Proto, r.SrcPort, destCell, rFixedW, expectedFixed)
		}
	}
}
