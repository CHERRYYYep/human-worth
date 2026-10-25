package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	contentpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/content/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Agent Self-Claimed bounded complete set: never truncate a voting directory.
const VotingCatalogBytes = 2 << 20
const VotingCatalogEntries = 4096

// Reserve the public null/default fields added per entry by the HTTP mapper.
const votingCatalogEnvelope = 4096 + 64*VotingCatalogEntries

func CatalogHash(c *contentpb.VotingCatalogSnapshot) string {
	copy := proto.Clone(c).(*contentpb.VotingCatalogSnapshot)
	copy.Sha256 = ""
	raw, _ := (proto.MarshalOptions{Deterministic: true}).Marshal(copy)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func ValidateCatalog(c *contentpb.VotingCatalogSnapshot) error {
	if c == nil || c.TaskId == "" || c.Generation < 1 || (c.State != "open" && c.State != "closed" && c.State != "unpublished") || len(c.Entries) > VotingCatalogEntries || proto.Size(c) > VotingCatalogBytes-votingCatalogEnvelope {
		return errors.New("catalog_too_large_or_invalid")
	}
	if c.State != "open" && len(c.Entries) != 0 {
		return errors.New("invalid_catalog")
	}
	seen := make(map[string]bool, len(c.Entries))
	for _, e := range c.Entries {
		if e == nil || e.TaskId != c.TaskId || e.Id == "" || seen[e.Id] || e.Author.GetId() == "" || (e.Side != "human" && e.Side != "agent") {
			return errors.New("invalid_catalog")
		}
		seen[e.Id] = true
	}
	if c.Sha256 != CatalogHash(c) {
		return errors.New("catalog_hash_conflict")
	}
	// Include encoding/json HTML escaping as used by HTTP, not just Proto size.
	raw, err := protojson.Marshal(c)
	if err != nil {
		return err
	}
	var value any
	if err = json.Unmarshal(raw, &value); err != nil {
		return err
	}
	raw, err = json.Marshal(value)
	if err != nil {
		return err
	}
	if len(raw) > VotingCatalogBytes-votingCatalogEnvelope {
		return errors.New("catalog_too_large")
	}
	return nil
}
