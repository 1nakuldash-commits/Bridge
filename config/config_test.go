package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAddOrUpdatePeerSkipsSaveWhenPersistentFieldsUnchanged(t *testing.T) {
	tempDir := t.TempDir()
	cfgPath := filepath.Join(tempDir, "config.json")

	cfg := DefaultConfig()
	cfg.ConfigPath = cfgPath
	cfg.NodeName = "U1"
	cfg.Configured = true
	if err := SaveConfig(cfgPath, cfg); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	// First add must persist
	dev, isNew := cfg.AddOrUpdatePeer("U2", "100.64.0.2", "http://100.64.0.2:8765")
	if !isNew || dev.Name != "U2" {
		t.Fatalf("expected new peer U2, got isNew=%v dev=%+v", isNew, dev)
	}

	// Backdate file modtime + record exact file bytes
	oldTime := time.Now().Add(-10 * time.Second).Truncate(time.Second)
	if err := os.Chtimes(cfgPath, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes failed: %v", err)
	}
	beforeBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	// Repeated authenticated call with unchanged persistent fields (and empty IP) must NOT rewrite config.json
	dev2, isNew2 := cfg.AddOrUpdatePeer("U2", "", "http://100.64.0.2:8765")
	if isNew2 {
		t.Fatalf("expected existing peer U2, got isNew=true")
	}
	if !dev2.Online || dev2.LastSeen.IsZero() {
		t.Fatalf("expected runtime state Online=true and non-zero LastSeen, got %+v", dev2)
	}

	infoAfter, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if !infoAfter.ModTime().Equal(oldTime) {
		t.Fatalf("expected config.json not to be rewritten on unchanged peer fields; modtime changed from %v to %v", oldTime, infoAfter.ModTime())
	}
	afterBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("ReadFile after failed: %v", err)
	}
	if string(beforeBytes) != string(afterBytes) {
		t.Fatalf("expected config.json bytes to remain identical")
	}

	// Changing a persistent field (Name) MUST persist
	dev3, isNew3 := cfg.AddOrUpdatePeer("U2-Renamed", "", "http://100.64.0.2:8765")
	if isNew3 || dev3.Name != "U2-Renamed" {
		t.Fatalf("expected updated peer name U2-Renamed, got isNew=%v dev=%+v", isNew3, dev3)
	}
	updatedBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("ReadFile updated failed: %v", err)
	}
	if !strings.Contains(string(updatedBytes), "U2-Renamed") {
		t.Fatalf("expected renamed peer to be saved to config.json, got:\n%s", string(updatedBytes))
	}
}

func TestSaveConfigAtomicAndNoTempLeftovers(t *testing.T) {
	tempDir := t.TempDir()
	cfgPath := filepath.Join(tempDir, "config.json")

	cfg := DefaultConfig()
	cfg.ConfigPath = cfgPath
	cfg.NodeName = "Unit-Atomic"
	cfg.Configured = true

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			c := DefaultConfig()
			c.ConfigPath = cfgPath
			c.NodeName = "Unit-Atomic"
			c.Configured = true
			c.ListenPort = 8765 + idx
			if err := SaveConfig(cfgPath, c); err != nil {
				t.Errorf("concurrent SaveConfig failed: %v", err)
			}
		}(i)
	}
	wg.Wait()

	// Must always load valid JSON with zero leftover temp files
	loaded, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig after concurrent atomic saves failed: %v", err)
	}
	if loaded.NodeName != "Unit-Atomic" || !loaded.Configured {
		t.Fatalf("unexpected loaded config: %+v", loaded)
	}

	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "config.json" {
			t.Fatalf("unexpected leftover file in config dir: %s", e.Name())
		}
	}
}

func TestLoadConfigPreservesExplicitConfiguredTrue(t *testing.T) {
	tempDir := t.TempDir()

	cases := []struct {
		name           string
		jsonText       string
		wantConfigured bool
	}{
		{
			name:           "Explicit configured true with Desktop 1",
			jsonText:       `{"configured":true,"node_name":"Desktop 1","listen_port":8765,"auth_token":"tok"}`,
			wantConfigured: true,
		},
		{
			name:           "Explicit configured true with Desktop 2",
			jsonText:       `{"configured":true,"node_name":"Desktop 2","listen_port":8765,"auth_token":"tok"}`,
			wantConfigured: true,
		},
		{
			name:           "Explicit configured true with Bridge-PC",
			jsonText:       `{"configured":true,"node_name":"Bridge-PC","listen_port":8765,"auth_token":"tok"}`,
			wantConfigured: true,
		},
		{
			name:           "Explicit configured true with custom name",
			jsonText:       `{"configured":true,"node_name":"Office-Unit","listen_port":8765,"auth_token":"tok"}`,
			wantConfigured: true,
		},
		{
			name:           "Explicit configured true with empty node_name still triggers setup",
			jsonText:       `{"configured":true,"node_name":"","listen_port":8765,"auth_token":"tok"}`,
			wantConfigured: false,
		},
		{
			name:           "Missing configured flag triggers setup",
			jsonText:       `{"node_name":"Desktop 1","listen_port":8765,"auth_token":"tok"}`,
			wantConfigured: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(tempDir, strings.ReplaceAll(tc.name, " ", "_")+".json")
			if err := os.WriteFile(p, []byte(tc.jsonText), 0644); err != nil {
				t.Fatalf("WriteFile failed: %v", err)
			}
			cfg, err := LoadConfig(p)
			if err != nil {
				t.Fatalf("LoadConfig failed: %v", err)
			}
			if cfg.Configured != tc.wantConfigured {
				t.Fatalf("LoadConfig(%s): Configured = %v, want %v", tc.name, cfg.Configured, tc.wantConfigured)
			}
		})
	}
}
