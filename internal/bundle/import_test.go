package bundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type bundleFetcher map[string]string

func (f bundleFetcher) Request(_ context.Context, method, path string, _ io.Reader, out io.Writer) error {
	if method != "GET" {
		return errors.New("unexpected export method")
	}
	value, ok := f[path]
	if !ok {
		return errors.New("unexpected export path: " + path)
	}
	_, err := io.WriteString(out, value)
	return err
}

type importCall struct {
	method string
	path   string
	body   string
}

type importAPI struct {
	calls []importCall
}

func (a *importAPI) Request(_ context.Context, method, path string, body io.Reader, out io.Writer) error {
	var payload strings.Builder
	if body != nil {
		_, _ = io.Copy(&payload, body)
	}
	a.calls = append(a.calls, importCall{method: method, path: path, body: payload.String()})
	switch {
	case method == "GET" && strings.HasPrefix(path, "agents/") && strings.HasSuffix(path, "/memory?limit=1"):
		_, _ = io.WriteString(out, `{"versions":[]}`)
	case method == "GET" && strings.HasPrefix(path, "agents/"):
		_, _ = io.WriteString(out, `{"agent":{"name":"target"}}`)
	case method == "POST" && strings.HasSuffix(path, "/import"):
		var request struct {
			Entries  []json.RawMessage `json:"entries"`
			Skills   []json.RawMessage `json:"skills"`
			Archives []json.RawMessage `json:"archives"`
		}
		_ = json.Unmarshal([]byte(payload.String()), &request)
		count := max(len(request.Entries), len(request.Skills), len(request.Archives))
		results := make([]map[string]string, count)
		for i := range results {
			results[i] = map[string]string{"outcome": "unchanged"}
		}
		response, _ := json.Marshal(map[string]any{"results": results})
		_, _ = out.Write(response)
	default:
		_, _ = io.WriteString(out, `{}`)
	}
	return nil
}

func portableConversationTestBundle(t *testing.T) string {
	t.Helper()
	const id = "10000000-0000-4000-8000-000000000001"
	fetch := bundleFetcher{
		"agents/source":                                         `{"agent":{"name":"source","spec":{}}}`,
		"agents/source/definitions":                             `{"name":"source","origin":"database","versions":[]}`,
		"agents/source/memory?limit=1":                          `{"versions":[]}`,
		"agents/source/context":                                 `{"topics":[],"skills":[]}`,
		"memory/entries?limit=1000&scope=profile&status=active": `{"entries":[]}`,
		"memory/entries?agent=source&limit=1000&status=active":  `{"entries":[]}`,
		"agents/source/conversations?archived=false&limit=200":  `{"conversations":[],"has_more":false,"next_before":null}`,
		"agents/source/conversations?archived=true&limit=200":   `{"conversations":[{"id":"` + id + `","agent":"source","title":"Old","archived":true}],"has_more":false,"next_before":null}`,
		"conversations/" + id + "/thread?limit=200":             `{"items":[{"kind":"message","id":"m1","body":"hello"}],"has_more":false,"next_before":null}`,
	}
	path := filepath.Join(t.TempDir(), "conversation-bundle.zip")
	if manifest, err := Export(context.Background(), fetch, "source", path); err != nil {
		t.Fatalf("%v: %+v", err, manifest.Resources)
	}
	return path
}

func portableTestBundle(t *testing.T) string {
	t.Helper()
	fetch := bundleFetcher{
		"agents/source":                                         `{"agent":{"name":"source","spec":{}}}`,
		"agents/source/definitions":                             `{"name":"source","origin":"database","versions":[]}`,
		"agents/source/memory?limit=1":                          `{"versions":[{"content":"memory text"}]}`,
		"agents/source/context":                                 `{"topics":[{"name":"finance","from":["api"]}],"skills":[]}`,
		"memory/entries?limit=1000&scope=profile&status=active": `{"entries":[{"scope":"profile","key":"pref","type":"user","title":"Preference","body":"tea"}]}`,
		"memory/entries?agent=source&limit=1000&status=active":  `{"entries":[{"scope":"agent","agent":"source","key":"local","type":"project","title":"Local","body":"source-only"}]}`,
		"memory/entries?limit=1000&status=active&topic=finance": `{"entries":[{"scope":"topic","topic":"finance","key":"topic","type":"reference","title":"Topic","body":"fact"}]}`,
		"agents/source/conversations?archived=false&limit=200":  `{"conversations":[],"has_more":false,"next_before":null}`,
		"agents/source/conversations?archived=true&limit=200":   `{"conversations":[],"has_more":false,"next_before":null}`,
	}
	path := filepath.Join(t.TempDir(), "bundle.zip")
	if _, err := Export(context.Background(), fetch, "source", path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestImportDryRunValidatesWithoutWrites(t *testing.T) {
	api := &importAPI{}
	result, err := Import(context.Background(), api, portableTestBundle(t), "target", true)
	if err != nil {
		t.Fatal(err)
	}
	if !result.DryRun || result.MemoryDocument != "would import" || result.ConversationArchivesImported || len(result.Attachments) != 1 {
		t.Fatalf("result: %+v", result)
	}
	var entryCalls int
	for _, call := range api.calls {
		if call.method == "POST" && call.path == "memory/import" {
			entryCalls++
			if !strings.Contains(call.body, `"dry_run":true`) || !strings.Contains(call.body, `"agent":"target"`) {
				t.Fatalf("bad memory dry run: %s", call.body)
			}
		}
		if call.method == "POST" && call.path == "agents/target/memory" {
			t.Fatal("dry run wrote memory")
		}
	}
	if entryCalls != 1 {
		t.Fatalf("entry calls %d", entryCalls)
	}
}

func TestImportArchivesConversationAsReadOnlySnapshot(t *testing.T) {
	api := &importAPI{}
	filename := portableConversationTestBundle(t)
	result, err := Import(context.Background(), api, filename, "target", false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.ConversationArchivesImported || result.ConversationArchivesCount != 1 {
		t.Fatalf("conversation import result: %+v", result)
	}
	var found bool
	for _, call := range api.calls {
		if call.path != "agents/target/conversation-archives/import" {
			continue
		}
		found = true
		var body struct {
			Archives []struct {
				SourceAgent          string `json:"source_agent"`
				SourceConversationID string `json:"source_conversation_id"`
				SourceBundleSHA256   string `json:"source_bundle_sha256"`
				Snapshot             struct {
					Conversation struct {
						ID string `json:"id"`
					} `json:"conversation"`
					Pages []json.RawMessage `json:"pages"`
				} `json:"snapshot"`
			} `json:"archives"`
		}
		if err := json.Unmarshal([]byte(call.body), &body); err != nil || len(body.Archives) != 1 {
			t.Fatalf("archive request body %q: %v", call.body, err)
		}
		archive := body.Archives[0]
		bundleBytes, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		bundleDigest := sha256.Sum256(bundleBytes)
		if archive.SourceAgent != "source" || archive.SourceConversationID != "10000000-0000-4000-8000-000000000001" ||
			archive.SourceBundleSHA256 != hex.EncodeToString(bundleDigest[:]) || archive.Snapshot.Conversation.ID != archive.SourceConversationID || len(archive.Snapshot.Pages) != 1 {
			t.Fatalf("archive payload: %+v", archive)
		}
	}
	if !found {
		t.Fatal("conversation archive endpoint was not called")
	}
}

func TestImportAppliesAndReportsConversationLimitation(t *testing.T) {
	api := &importAPI{}
	result, err := Import(context.Background(), api, portableTestBundle(t), "target", false)
	if err != nil {
		t.Fatal(err)
	}
	if result.MemoryDocument != "imported" || result.ConversationArchivesImported {
		t.Fatalf("result: %+v", result)
	}
	var wroteMemory, attachedTopic bool
	for _, call := range api.calls {
		if call.method == "POST" && call.path == "agents/target/memory" {
			wroteMemory = true
		}
		if call.method == "PUT" && call.path == "agents/target/topics/finance" {
			attachedTopic = true
		}
	}
	if !wroteMemory || !attachedTopic {
		t.Fatalf("memory=%v topic=%v calls=%+v", wroteMemory, attachedTopic, api.calls)
	}
}

func TestImportRefusesIncompleteBeforeTargetCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.zip")
	_, exportErr := Export(context.Background(), bundleFetcher{}, "source", path)
	if !errors.Is(exportErr, ErrIncomplete) {
		t.Fatalf("export err %v", exportErr)
	}
	api := &importAPI{}
	if _, err := Import(context.Background(), api, path, "target", false); err == nil || len(api.calls) != 0 {
		t.Fatalf("err=%v calls=%v", err, api.calls)
	}
}
