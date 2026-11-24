package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
)

type listenerConfig struct {
	Bind string `json:"bind"`
	Port int    `json:"port"`
}

func (p runtimePaths) mcpAddress() (netip.AddrPort, error) {
	path := filepath.Join(p.config, "config.json")
	cfg := &listenerConfig{Bind: "127.0.0.1", Port: 9417}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		defaults, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return netip.AddrPort{}, err
		}
		// Other login sessions can create or edit the file during first start.
		if err := publishPrivate(path, append(defaults, '\n'), os.Link); err != nil && !errors.Is(err, os.ErrExist) {
			return netip.AddrPort{}, fmt.Errorf("create %s: %w", path, err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return netip.AddrPort{}, err
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return netip.AddrPort{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := decoder.Decode(new(json.RawMessage)); cfg == nil || err != io.EOF {
		return netip.AddrPort{}, fmt.Errorf("%s: expected one JSON object", path)
	}
	if cfg.Port < 0 || cfg.Port > 65535 {
		return netip.AddrPort{}, fmt.Errorf("%s: port must be between 0 and 65535", path)
	}
	host, err := mcpBindAddress(cfg.Bind)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("%s: %w", path, err)
	}
	return netip.AddrPortFrom(host, uint16(cfg.Port)), nil
}
