package secret

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// goldenRoot is the root the golden values in this repository were recorded under, from the derivations orbit shipped before this package existed. Links, reply addresses, upload URLs and sealed OAuth state already sent out verify only while these stay byte-identical.
const goldenRoot = "0123456789abcdef0123456789abcdef-golden"

func TestDeriveIsByteIdenticalToWhatOrbitAlreadySigned(t *testing.T) {
	k := Key(goldenRoot)
	for purpose, want := range map[string]string{
		Approval("7a1c2d3e-0000-4000-8000-000000000001"): "a1ba81769f93cc542236f4f5698826553c5a38d5252de274a1d29db9172ef66b",
		Mail("7a1c2d3e000040008000000000000002"):         "beda42d06e1b",
		MCPOAuth:                                         "75d7c67af365412b7774b47fe08f677863a45b6bc016af42c563a2f026439054",
		PushAction:                                       "183cc4381862674164b51f6f51889dc053f8b96ecccd253248b145e5ee4cf2dc",
	} {
		if got := hex.EncodeToString(k.Derive(purpose)); got[:len(want)] != want {
			t.Errorf("Derive(%q) = %s, want %s", purpose, got, want)
		}
	}
}

func TestPurposesAndRootsSeparateKeys(t *testing.T) {
	k := Key(goldenRoot)
keys := [][]byte{k.Derive(UploadURL), k.Derive(WorkspaceUploadURL), k.Derive(MCPOAuth), k.Derive(TemplateLink), k.Derive(ConnectorTokens), k.Derive(PushAction), k.Derive(Approval("x")), k.Derive(Mail("x"))}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if bytes.Equal(keys[i], keys[j]) {
				t.Errorf("purposes %d and %d share a key", i, j)
			}
		}
	}
	if bytes.Equal(k.Derive(UploadURL), Key("another root").Derive(UploadURL)) {
		t.Error("two roots derive the same key")
	}
}
