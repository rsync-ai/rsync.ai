package main

import (
	"strings"
	"testing"
)

// A connection pinned to a destination version must be served by that version's
// container or not at all. The unversioned aliases (`<type>-mcp`,
// `rsync-ai-<type>-mcp`) name whichever container holds the alias, so falling
// back to one could write a pinned v1.0.0 connection through a different version.
// The versioned name also has to carry STACK_PREFIX: on a prefixed stack
// (rsync-ci) the hard-coded `rsync-ai-` name is another stack's container.

func TestDestinationHostCandidates_PinnedVersionNeverFallsBackToAnUnversionedAlias(t *testing.T) {
	t.Setenv("STACK_PREFIX", "")
	for _, h := range destinationHostCandidates("postgresql", "1.0.0") {
		if !strings.Contains(h, "-v1-0-0-") {
			t.Errorf("pinned 1.0.0 candidates include %q, an alias that does not name the version", h)
		}
	}
	hosts := destinationHostCandidates("postgresql", "v1.0.0")
	if len(hosts) == 0 || hosts[0] != "rsync-ai-postgresql-v1-0-0-mcp" {
		t.Fatalf("candidates = %v, want the versioned container first", hosts)
	}
}

func TestDestinationHostCandidates_VersionedNameCarriesTheStackPrefix(t *testing.T) {
	t.Setenv("STACK_PREFIX", "rsync-ci")
	hosts := destinationHostCandidates("aws_s3", "1.0.0")
	if len(hosts) == 0 || hosts[0] != "rsync-ci-aws-s3-v1-0-0-mcp" {
		t.Fatalf("candidates = %v, want rsync-ci-aws-s3-v1-0-0-mcp first", hosts)
	}
	for _, h := range hosts {
		if strings.HasPrefix(h, "rsync-ai-") {
			t.Errorf("STACK_PREFIX=rsync-ci candidates include %q, another stack's container", h)
		}
	}
}

// No concrete version: nothing is pinned, so the aliases are the way in.
func TestDestinationHostCandidates_UnpinnedKeepsTheAliases(t *testing.T) {
	t.Setenv("STACK_PREFIX", "")
	for _, v := range []string{"", "latest"} {
		hosts := destinationHostCandidates("postgresql", v)
		joined := strings.Join(hosts, ",")
		for _, want := range []string{"postgresql-mcp", "rsync-ai-postgresql-mcp"} {
			if !strings.Contains(","+joined+",", ","+want+",") {
				t.Errorf("version %q: candidates = %v, want alias %q", v, hosts, want)
			}
		}
	}
}
