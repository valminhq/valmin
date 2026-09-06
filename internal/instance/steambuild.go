package instance

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/scanner"
)

const steamMetadataLimit = 1 << 20

type steamValue struct {
	text     string
	children map[string]steamValue
}

// PublicBuildID reads only the public branch, ignoring SteamCMD's diagnostic preamble.
func PublicBuildID(output string) (string, error) {
	if len(output) > steamMetadataLimit {
		return "", fmt.Errorf("steam metadata exceeds %d bytes", steamMetadataLimit)
	}
	offset := 0
	for line := range strings.Lines(output) {
		if strings.TrimSpace(line) == strconv.Quote(AppID) {
			record := output[offset:]
			root, err := readSteamRecord(record, AppID)
			if err != nil {
				return "", err
			}
			return validSteamBuild(
				root.children["depots"].children["branches"].children["public"].children["buildid"].text,
			)
		}
		offset += len(line)
	}
	return "", fmt.Errorf("steam metadata has no app %s record", AppID)
}

// InstalledBuildID reads the manifest through the instance root without following escaping symlinks.
func InstalledBuildID(dataDir string) (string, error) {
	root, err := os.OpenRoot(dataDir)
	if err != nil {
		return "", fmt.Errorf("open instance root: %w", err)
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open("server/steamapps/appmanifest_" + AppID + ".acf")
	if err != nil {
		return "", fmt.Errorf("open installed manifest: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, steamMetadataLimit+1))
	if err != nil {
		return "", fmt.Errorf("read installed manifest: %w", err)
	}
	return ManifestBuildID(string(data))
}

func ManifestBuildID(data string) (string, error) {
	root, err := readSteamRecord(data, "AppState")
	if err != nil {
		return "", err
	}
	if root.children["appid"].text != AppID {
		return "", fmt.Errorf("installed manifest is not app %s", AppID)
	}
	return validSteamBuild(root.children["buildid"].text)
}

func validSteamBuild(id string) (string, error) {
	if id == "" || strings.Trim(id, "0123456789") != "" {
		return "", fmt.Errorf("steam build id is missing or invalid")
	}
	if n, err := strconv.ParseUint(id, 10, 64); err != nil || n == 0 {
		return "", fmt.Errorf("steam build id is not a positive integer")
	}
	return id, nil
}

func readSteamRecord(data, name string) (steamValue, error) {
	if len(data) > steamMetadataLimit {
		return steamValue{}, fmt.Errorf("steam metadata exceeds %d bytes", steamMetadataLimit)
	}
	var s scanner.Scanner
	s.Init(strings.NewReader(data))
	s.Mode = scanner.ScanStrings | scanner.ScanComments | scanner.SkipComments
	s.Error = func(_ *scanner.Scanner, _ string) {}
	if s.Scan() != scanner.String || s.TokenText() != strconv.Quote(name) || s.Scan() != '{' {
		return steamValue{}, fmt.Errorf("invalid steam %s record", name)
	}
	children, err := readSteamObject(&s, 0)
	if err != nil {
		return steamValue{}, err
	}
	if s.ErrorCount != 0 {
		return steamValue{}, fmt.Errorf("invalid steam string")
	}
	return steamValue{children: children}, nil
}

func readSteamObject(s *scanner.Scanner, depth int) (map[string]steamValue, error) {
	if depth > 32 {
		return nil, fmt.Errorf("steam metadata nesting exceeds 32")
	}
	values := make(map[string]steamValue)
	for token := s.Scan(); token != '}'; token = s.Scan() {
		if token != scanner.String {
			return nil, fmt.Errorf("expected steam metadata key")
		}
		key, err := strconv.Unquote(s.TokenText())
		if err != nil {
			return nil, fmt.Errorf("decode steam key: %w", err)
		}
		if _, exists := values[key]; exists {
			return nil, fmt.Errorf("duplicate steam metadata key")
		}
		value, err := readSteamValue(s, depth)
		if err != nil {
			return nil, err
		}
		values[key] = value
	}
	return values, nil
}

func readSteamValue(s *scanner.Scanner, depth int) (steamValue, error) {
	switch s.Scan() {
	case scanner.String:
		value, err := strconv.Unquote(s.TokenText())
		if err != nil {
			return steamValue{}, fmt.Errorf("decode steam value: %w", err)
		}
		return steamValue{text: value}, nil
	case '{':
		children, err := readSteamObject(s, depth+1)
		return steamValue{children: children}, err
	default:
		return steamValue{}, fmt.Errorf("expected steam metadata value")
	}
}
