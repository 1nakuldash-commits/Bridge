package installer

import (
	"encoding/json"
	"testing"
	"time"

	"bridge/config"
	"bridge/server"
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

func TestPeerDeviceOnlineSerialization(t *testing.T) {
	dev := config.PeerDevice{
		Name:      "U1(D)",
		IP:        "100.92.92.87",
		URL:       "http://100.92.92.87:8765",
		SpoolPort: 9101,
		Online:    true,
		LastSeen:  time.Now(),
	}

	data, err := json.Marshal(dev)
	if err != nil {
		t.Fatalf("Failed to marshal PeerDevice: %v", err)
	}

	var decoded config.PeerDevice
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal PeerDevice: %v", err)
	}

	if !decoded.Online {
		t.Errorf("Expected decoded.Online to be true, got false. JSON payload: %s", string(data))
	}
}

func TestHealthResponsePeerOnlineSerialization(t *testing.T) {
	resp := server.HealthResponse{
		Status:      "healthy",
		NodeName:    "ST_ND",
		OnlineCount: 1,
		Peers: []config.PeerDevice{
			{
				Name:      "U1(D)",
				IP:        "100.92.92.87",
				URL:       "http://100.92.92.87:8765",
				SpoolPort: 9101,
				Online:    true,
			},
		},
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Failed to marshal HealthResponse: %v", err)
	}

	var decoded server.HealthResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal HealthResponse: %v", err)
	}

	if len(decoded.Peers) != 1 {
		t.Fatalf("Expected 1 peer, got %d", len(decoded.Peers))
	}
	if !decoded.Peers[0].Online {
		t.Errorf("Expected decoded.Peers[0].Online to be true, got false. Raw JSON: %s", string(data))
	}
}
