package comms

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseRegistryByte(t *testing.T) {
	golden := strings.Join([]string{
		"m00",
		"",
		"00: ff ff ff ff ff 1e aa bb cc dd ee ff 01 02 03 04",
		"10: 00 00 03 1e ff ff ff ff ff ff ff ff ff ff ff ff",
		"20: ff ff ff ff ff ff ff ff ff ff ff ff ff ff ff ff",
		"",
		"O^K",
	}, "\r\n")

	mixed := string([]byte{0xa5, 0xfc}) + "\r\nO^K\r\n" + golden

	tests := []struct {
		name    string
		resp    string
		reg     int
		want    int64
		wantErr bool
	}{
		{
			name:    "empty",
			resp:    "",
			reg:     0x05,
			wantErr: true,
		},
		{
			name:    "short garbage",
			resp:    "\r",
			reg:     0x05,
			wantErr: true,
		},
		{
			name:    "no dump lines",
			resp:    "\r\nO^K\r\n",
			reg:     0x05,
			wantErr: true,
		},
		{
			name: "prediction lockout reg 0x05",
			resp: golden,
			reg:  0x05,
			want: 0x1e, // 30 mins
		},
		{
			name: "battery hours reg 0x12",
			resp: golden,
			reg:  0x12,
			want: 0x03,
		},
		{
			name: "battery mins reg 0x13",
			resp: golden,
			reg:  0x13,
			want: 0x1e,
		},
		{
			name: "ff treated as unset zero",
			resp: golden,
			reg:  0x00,
			want: 0,
		},
		{
			name: "mixed prefix noise still parses",
			resp: mixed,
			reg:  0x05,
			want: 0x1e,
		},
		{
			name: "uppercase hex line",
			resp: "00: FF FF FF FF FF 0A 00 00 00 00 00 00 00 00 00 00\r\n",
			reg:  0x05,
			want: 0x0a,
		},
		{
			name:    "column past end of short line",
			resp:    "00: ff ff\r\n",
			reg:     0x05,
			wantErr: true,
		},
		{
			// An m10 dump the node labelled relative to the request: the only row
			// present is row 1, so it is read despite the 00: label.
			name: "row relabelled to dump start",
			resp: "m10\r\n\r\n00: 00 00 03 1e ff ff ff ff ff ff ff ff ff ff ff ff\r\n\r\nO^K\r\n",
			reg:  0x13,
			want: 0x1e,
		},
		{
			name: "echoed command is not mistaken for a row",
			resp: "m00\r\n00: ff ff ff ff ff 1e ff ff ff ff ff ff ff ff ff ff\r\nO^K\r\n",
			reg:  0x05,
			want: 0x1e,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRegistryByte([]byte(tt.resp), tt.reg)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got value %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %d want %d", got, tt.want)
			}
		})
	}
}

func TestFormatRegistryRowHighlightsSelectedColumn(t *testing.T) {
	fields := strings.Fields("10: 00 00 03 1e ff ff ff ff ff ff ff ff ff ff ff ff")
	got := formatRegistryRow(fields, 2)
	want := "10: 00 00 [03] 1e ff ff ff ff ff ff ff ff ff ff ff ff"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestCalcCRC16Stable(t *testing.T) {
	// Ensure AT+XCMD payload CRC helper still returns two bytes.
	crc := calcCRC16([]byte("m00"))
	if len(crc) != 2 {
		t.Fatalf("expected 2 CRC bytes, got %d", len(crc))
	}
}

func TestRegistryDumpCommandUsesRowStart(t *testing.T) {
	tests := []struct {
		reg  int
		want string
	}{
		{reg: 0x00, want: "m00"},
		{reg: 0x05, want: "m00"},
		{reg: 0x12, want: "m10"},
		{reg: 0x13, want: "m10"},
		{reg: 0xff, want: "mf0"},
	}

	for _, tt := range tests {
		if got := registryDumpCommand(tt.reg); got != tt.want {
			t.Errorf("registryDumpCommand(0x%02x) = %q, want %q", tt.reg, got, tt.want)
		}
	}
}

// Row-aligned commands happen to have CRCs free of flag/CR/LF/ESC, but that is
// not why we send m10 instead of m12 — the node only uses the high nibble.
// Stuffing is what makes a CRC that does contain those bytes (e.g. m12) safe.
func TestByteStuffXCMDEscapesFramingBytes(t *testing.T) {
	in := []byte{0x01, 0x7e, '\r', '\n', 0x1b, 0x02}
	got := byteStuffXCMD(in)
	want := []byte{0x01, 0x7e, 0x7e, 0x7e, '\r', 0x7e, '\n', 0x7e, 0x1b, 0x02}
	if string(got) != string(want) {
		t.Fatalf("got %q want %q", got, want)
	}
	if string(byteStuffXCMD([]byte("m00"))) != "m00" {
		t.Fatal("ASCII with no framing bytes should be unchanged")
	}
}

func TestEncodeATXCMDStuffsCRCFlag(t *testing.T) {
	// m12 CRC is 0xea,0x7e; without stuffing the node sees CH_FLAG and drops the byte.
	crc := calcCRC16([]byte("m12"))
	if len(crc) != 2 || crc[1] != 0x7e {
		t.Fatalf("m12 CRC = %x, expected ..7e so this test still covers stuffing", crc)
	}
	got := encodeATXCMD("m12")
	want := append([]byte("AT+XCMD=m12"), crc[0], 0x7e, crc[1])
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}

	plain := encodeATXCMD("m10")
	if bytes.Contains(plain, []byte{0x7e}) {
		t.Fatalf("m10 encoding unexpectedly contains flag: %q", plain)
	}
}
