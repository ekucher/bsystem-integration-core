// Command checkhostnames fails when any file in this repository references a
// hostname that is neither a reserved documentation domain (RFC 2606/6761)
// nor an explicitly allowed real service.
//
// Why this exists: a real stage deployment hostname and a maintainer's own
// real domain both ended up committed in a sibling repository during a
// Stage 1 SSO round. This is the same check, ported to this repo's
// dependency-free Go tooling convention (see cmd/mapping-audit).
//
// Run: go run ./cmd/checkhostnames
package main

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Real services this repo's docs/configs legitimately reference.
var allowedHosts = map[string]bool{
	"127.0.0.1":  true,
	"0.0.0.0":    true,
	"github.com": true,
}

// RFC 2606 / RFC 6761 reserve these for documentation and examples.
var reservedHostRE = regexp.MustCompile(`(?i)(?:^|\.)(?:example|invalid|test|localhost)(?:\.[a-z]{2,})?$`)

var hostRE = regexp.MustCompile(`https?://([a-zA-Z0-9][a-zA-Z0-9.-]*)`)

// Directories that are either version control internals or would only ever
// contain generated/vendored content no human wrote by hand.
var excludeDir = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
}

type finding struct {
	path string
	line int
	host string
}

func main() {
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var findings []finding
	fileCount := 0

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if excludeDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		fileCount++

		f, ferr := os.Open(path)
		if ferr != nil {
			return nil // unreadable -- not source a human wrote by hand
		}
		defer f.Close()

		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)

		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		lineno := 0
		for scanner.Scan() {
			lineno++
			line := scanner.Text()
			for _, m := range hostRE.FindAllStringSubmatch(line, -1) {
				host := strings.ToLower(strings.SplitN(m[1], ":", 2)[0])
				// A bare single label (no dot) is never a real,
				// publicly-resolvable domain -- an obvious placeholder.
				if !strings.Contains(host, ".") {
					continue
				}
				if allowedHosts[host] {
					continue
				}
				if reservedHostRE.MatchString(host) {
					continue
				}
				findings = append(findings, finding{path: rel, line: lineno, host: host})
			}
		}
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if len(findings) > 0 {
		fmt.Println("Found real-looking hostnames committed to the repo:")
		fmt.Println()
		for _, f := range findings {
			fmt.Printf("  %s:%d: host %q is not a reserved documentation domain or an allowed real service\n", f.path, f.line, f.host)
		}
		fmt.Println()
		fmt.Printf("%d finding(s). Use a reserved documentation domain instead "+
			"(e.g. core.example.com, *.example.invalid), or add the host to "+
			"allowedHosts in cmd/checkhostnames/main.go if it is a real "+
			"third-party service this repo legitimately references.\n", len(findings))
		os.Exit(1)
	}

	fmt.Printf("checked %d file(s): no committed real-looking hostname\n", fileCount)
}
