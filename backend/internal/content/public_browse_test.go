package content

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestEntryCursor(t *testing.T) {
	task := "tsk_" + strings.Repeat("a", 32)
	id := "ent_" + strings.Repeat("b", 32)
	raw := encodeEntryCursor(task, 7, 3, id)
	c, err := decodeEntryCursor(raw, task)
	if err != nil || c.Position != 3 || c.SourceRevision != 7 || c.ID != id {
		t.Fatalf("roundtrip: %+v %v", c, err)
	}
	c, err = decodeEntryCursor("", task)
	if err != nil || c.Position != -1 {
		t.Fatal("empty is first page", err)
	}
	for _, bad := range []string{"!", strings.Repeat("x", 2049), raw + "="} {
		if _, err := decodeEntryCursor(bad, task); status.Code(err) != codes.InvalidArgument {
			t.Fatal("invalid accepted", err)
		}
	}
	for _, edit := range []func(*entryCursor){func(c *entryCursor) { c.Version = 2 }, func(c *entryCursor) { c.Task = "other" }, func(c *entryCursor) { c.Sort = "other" }, func(c *entryCursor) { c.SourceRevision = 0 }, func(c *entryCursor) { c.Position = -1 }, func(c *entryCursor) { c.Position = 32 }, func(c *entryCursor) { c.ID = "tsk_bad" }} {
		c, _ := decodeEntryCursor(raw, task)
		edit(&c)
		data, _ := json.Marshal(c)
		if _, err := decodeEntryCursor(base64.RawURLEncoding.EncodeToString(data), task); status.Code(err) != codes.InvalidArgument {
			t.Fatal("invalid scope accepted", err)
		}
	}
	data, _ := base64.RawURLEncoding.DecodeString(raw)
	duplicate := strings.TrimSuffix(string(data), "}") + `,"v":1}`
	if _, err := decodeEntryCursor(base64.RawURLEncoding.EncodeToString([]byte(duplicate)), task); status.Code(err) != codes.InvalidArgument {
		t.Fatal("duplicate cursor fields accepted")
	}
}
