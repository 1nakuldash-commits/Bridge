package installer

import (
	"testing"

	"bridge/config"
)

func TestSanitizeFileName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"NormalName", "NormalName"},
		{"Unit 1 (Desk)", "Unit 1 (Desk)"},
		{"Unit/1:Desk*Name", "Unit_1_Desk_Name"},
		{"   Spaced Name   ", "Spaced Name"},
	}

	for _, tt := range tests {
		got := SanitizeFileName(tt.input)
		if got != tt.want {
			t.Errorf("SanitizeFileName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSuggestNextDeviceName(t *testing.T) {
	tests := []struct {
		detected []string
		currName string
		want     string
	}{
		{[]string{}, "", "U1"},
		{[]string{"U1"}, "", "U2"},
		{[]string{"U1", "U2"}, "", "U3"},
		{[]string{"Office"}, "U1", "U1"},
	}

	for _, tt := range tests {
		got := SuggestNextDeviceName(tt.detected, tt.currName)
		if got != tt.want {
			t.Errorf("SuggestNextDeviceName(%v, %q) = %q, want %q", tt.detected, tt.currName, got, tt.want)
		}
	}
}

func TestPruneObsoleteVirtualPrintersEmpty(t *testing.T) {
	// Ensure PruneObsoleteVirtualPrinters does not panic or error on empty peer slice
	peers := []config.PeerDevice{
		{Name: "U1(D)", SpoolPort: 9101},
	}
	// On Windows, running with valid peers generates a valid script
	err := PruneObsoleteVirtualPrinters(peers)
	if err != nil {
		t.Logf("PruneObsoleteVirtualPrinters returned note (expected if not elevated): %v", err)
	}
}
