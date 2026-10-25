package contract

import (
	contentpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/content/v1"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
)

func TestVotingCatalogCompleteValidation(t *testing.T) {
	c := &contentpb.VotingCatalogSnapshot{TaskId: "tsk_test", Generation: 1, State: "open", Entries: []*contentpb.ApprovedEntry{{Id: "ent_one", TaskId: "tsk_test", Side: "human", Author: &contentpb.PublicAuthor{Id: "author"}}}}
	c.Sha256 = CatalogHash(c)
	if err := ValidateCatalog(c); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*contentpb.VotingCatalogSnapshot){
		func(x *contentpb.VotingCatalogSnapshot) { x.Entries = append(x.Entries, x.Entries[0]) },
		func(x *contentpb.VotingCatalogSnapshot) { x.Entries[0].TaskId = "other" },
		func(x *contentpb.VotingCatalogSnapshot) { x.Entries[0].Author = nil },
		func(x *contentpb.VotingCatalogSnapshot) { x.State = "closed" },
		func(x *contentpb.VotingCatalogSnapshot) {
			x.Entries[0].Title = strings.Repeat("<", VotingCatalogBytes/5)
		},
		func(x *contentpb.VotingCatalogSnapshot) {
			x.Entries = make([]*contentpb.ApprovedEntry, VotingCatalogEntries+1)
		},
	} {
		x := proto.Clone(c).(*contentpb.VotingCatalogSnapshot)
		mutate(x)
		x.Sha256 = CatalogHash(x)
		if ValidateCatalog(x) == nil {
			t.Fatal("accepted invalid/incomplete/over-budget catalog")
		}
	}
	changed := proto.Clone(c).(*contentpb.VotingCatalogSnapshot)
	changed.Generation++
	if CatalogHash(changed) == c.Sha256 || ValidateCatalog(changed) == nil {
		t.Fatal("generation not bound to hash")
	}
	empty := &contentpb.VotingCatalogSnapshot{TaskId: "tsk_test", Generation: 2, State: "open"}
	empty.Sha256 = CatalogHash(empty)
	if err := ValidateCatalog(empty); err != nil {
		t.Fatal("empty complete set", err)
	}
}
