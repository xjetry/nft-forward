package daemon

import (
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"strings"
	"testing"

	"nft-forward/internal/wsproto"
)

func TestUpgradeBinaryFromData(t *testing.T) {
	payload := []byte("hello-binary")
	sum := sha256.Sum256(payload)
	good := wsproto.Upgrade{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(payload)), Data: payload}
	got, err := upgradeBinary(good)
	if err != nil {
		t.Fatalf("good data: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("got %q, want %q", got, payload)
	}

	bad := wsproto.Upgrade{SHA256: "deadbeef", Size: int64(len(payload)), Data: payload}
	if _, err := upgradeBinary(bad); err == nil {
		t.Fatal("sha mismatch should error")
	}
}

// elfHeader 造一个只含定长头部的 ELF 片段，足够 parseELFTarget 判定目标机器。
func elfHeader(class elf.Class, data elf.Data, machine elf.Machine) []byte {
	h := make([]byte, elfHeaderLen)
	copy(h, elf.ELFMAG)
	h[elf.EI_CLASS] = byte(class)
	h[elf.EI_DATA] = byte(data)
	if data == elf.ELFDATA2MSB {
		h[18], h[19] = byte(machine>>8), byte(machine)
	} else {
		h[18], h[19] = byte(machine), byte(machine>>8)
	}
	return h
}

func TestCheckUpgradeTarget(t *testing.T) {
	amd64 := elfHeader(elf.ELFCLASS64, elf.ELFDATA2LSB, elf.EM_X86_64)
	arm64 := elfHeader(elf.ELFCLASS64, elf.ELFDATA2LSB, elf.EM_AARCH64)
	// 升级包比头部长得多；后面的字节不该影响判定。
	withBody := func(h []byte) []byte { return append(append([]byte{}, h...), make([]byte, 4096)...) }

	accept := []struct {
		name     string
		bin, cur []byte
	}{
		{"amd64 on amd64", withBody(amd64), amd64},
		{"arm64 on arm64", withBody(arm64), arm64},
	}
	for _, tc := range accept {
		if err := checkUpgradeTarget(tc.bin, tc.cur); err != nil {
			t.Errorf("%s: same target must be accepted, got %v", tc.name, err)
		}
	}

	reject := []struct {
		name     string
		bin, cur []byte
		wantSub  string
	}{
		// 两个方向都钉：不分架构的面板把 amd64 推给 arm64 节点，以及反过来。
		{"amd64 onto arm64", withBody(amd64), arm64, "EM_X86_64"},
		{"arm64 onto amd64", withBody(arm64), amd64, "EM_AARCH64"},
		{"32-bit onto 64-bit", withBody(elfHeader(elf.ELFCLASS32, elf.ELFDATA2LSB, elf.EM_ARM)), arm64, "EM_ARM"},
		{"big-endian onto little-endian", withBody(elfHeader(elf.ELFCLASS64, elf.ELFDATA2MSB, elf.EM_AARCH64)), arm64, "ELFDATA2MSB"},
		{"not an ELF", []byte("#!/bin/sh\necho not a binary, long enough to pass the length check\n"), amd64, "upgrade binary: not an ELF"},
		{"truncated", amd64[:10], amd64, "upgrade binary: not an ELF"},
		{"empty", nil, amd64, "upgrade binary: not an ELF"},
		// 基准读不出来时不能放行：没有基准就无从证明升级包能在本机跑。
		{"running binary unreadable as ELF", withBody(amd64), []byte("\xcf\xfa\xed\xfe mach-o header bytes"), "running binary: not an ELF"},
	}
	for _, tc := range reject {
		err := checkUpgradeTarget(tc.bin, tc.cur)
		if err == nil {
			t.Errorf("%s: must be rejected", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantSub) {
			t.Errorf("%s: error %q should mention %q", tc.name, err, tc.wantSub)
		}
	}
}

// 大端头部的 e_machine 按大端读：同一机器类型换了字节序仍应被认出来，否则
// 字节序不同的两份头会被读成同一个数而漏判（或反之）。
func TestParseELFTargetHonoursEndianness(t *testing.T) {
	le, err := parseELFTarget(elfHeader(elf.ELFCLASS64, elf.ELFDATA2LSB, elf.EM_AARCH64))
	if err != nil {
		t.Fatal(err)
	}
	be, err := parseELFTarget(elfHeader(elf.ELFCLASS64, elf.ELFDATA2MSB, elf.EM_AARCH64))
	if err != nil {
		t.Fatal(err)
	}
	if le.machine != elf.EM_AARCH64 || be.machine != elf.EM_AARCH64 {
		t.Fatalf("machine must decode to EM_AARCH64 in both encodings, got le=%s be=%s", le.machine, be.machine)
	}
}
