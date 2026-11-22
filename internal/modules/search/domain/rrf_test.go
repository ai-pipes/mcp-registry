package domain_test

import (
	"reflect"
	"testing"

	"github.com/ai-pipes/mcp-registry/internal/modules/search/domain"
)

func ids(r []domain.Ranked) []string {
	out := make([]string, 0, len(r))
	for _, x := range r {
		out = append(out, x.ID)
	}
	return out
}

func TestRRFFusesTwoRankedLists(t *testing.T) {
	got := ids(domain.RRF([][]string{{"a", "b", "c"}, {"c", "a"}}, 60))
	if want := []string{"a", "c", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestRRFSumsOneOverKPlusRankWithOneBasedRanks(t *testing.T) {
	r := domain.RRF([][]string{{"a"}, {"x", "a"}}, 60)
	want := 1.0/61 + 1.0/62
	if d := r[0].Score - want; d > 1e-15 || d < -1e-15 {
		t.Fatalf("score %v, want %v", r[0].Score, want)
	}
}

func TestRRFKeepsFirstSeenOrderOnTiesAndHandlesEmptyInput(t *testing.T) {
	if got := ids(domain.RRF([][]string{{"a"}, {"b"}}, 60)); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("ties: %v", got)
	}
	// a tie of three, spread over lists: first-seen is the first list, then ids seen later
	if got := ids(domain.RRF([][]string{{"a", "c"}, {"b", "d"}}, 60)); !reflect.DeepEqual(got, []string{"a", "b", "c", "d"}) {
		t.Fatalf("interleaved ties: %v", got)
	}
	if got := domain.RRF(nil, 60); len(got) != 0 {
		t.Fatalf("nil: %v", got)
	}
	if got := domain.RRF([][]string{{}, {}}, 60); len(got) != 0 {
		t.Fatalf("empty lists: %v", got)
	}
}

func TestRRFCountsAnIDOncePerList(t *testing.T) {
	r := domain.RRF([][]string{{"a", "a"}}, 60)
	if len(r) != 1 || r[0].Score != 1.0/61 {
		t.Fatalf("got %v", r)
	}
	// the duplicate still occupies a rank: b is at position 3 of the list, so 1/63
	r = domain.RRF([][]string{{"a", "a", "b"}}, 60)
	if r[1].ID != "b" || r[1].Score != 1.0/63 {
		t.Fatalf("got %v", r)
	}
}

func TestRRFSortsByDescendingScore(t *testing.T) {
	got := ids(domain.RRF([][]string{{"x", "y", "z"}, {"z", "y"}}, 60))
	// z: 1/63+1/61 > y: 1/62+1/62 > x: 1/61
	if want := []string{"z", "y", "x"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Expected values computed with node: (1/(60+i)+1/(60+j))/(2/(60+1)) and (0+1/(60+i))/(2/(60+1)).
func TestScoreEqualsJavaScriptDoubles(t *testing.T) {
	pairs := []struct {
		i, j int
		want float64
	}{
		{1, 1, 1},
		{1, 2, 0.9919354838709679},
		{2, 1, 0.9919354838709679},
		{3, 7, 0.939350864723999},
		{10, 50, 0.712987012987013},
		{50, 50, 0.5545454545454545},
		{1, 50, 0.7772727272727272},
		{5, 23, 0.8367006487488415},
		{7, 7, 0.9104477611940298},
		{1, 3, 0.9841269841269842},
	}
	for _, p := range pairs {
		fts := make([]string, p.i)
		vec := make([]string, p.j)
		for n := range fts {
			fts[n] = "f" + itoa(n)
		}
		for n := range vec {
			vec[n] = "v" + itoa(n)
		}
		fts[p.i-1], vec[p.j-1] = "t", "t"
		hits := domain.Fuse(cands(fts), cands(vec), 1000)
		var got float64 = -1
		for _, h := range hits {
			if h.Ref == "t" {
				got = h.Score
			}
		}
		if got != p.want {
			t.Errorf("ranks (%d,%d): got %v, want %v", p.i, p.j, got, p.want)
		}
	}
	single := map[int]float64{1: 0.5, 2: 0.4919354838709677, 3: 0.4841269841269841, 10: 0.43571428571428567, 50: 0.2772727272727272}
	for i, want := range single {
		fts := make([]string, i)
		for n := range fts {
			fts[n] = "f" + itoa(n)
		}
		hits := domain.Fuse(cands(fts), nil, 1000)
		if got := hits[i-1].Score; got != want {
			t.Errorf("single rank %d: got %v, want %v", i, got, want)
		}
	}
}

func cands(refs []string) []domain.Candidate {
	out := make([]domain.Candidate, 0, len(refs))
	for _, r := range refs {
		out = append(out, domain.Candidate{Ref: r, Server: "srv-" + r})
	}
	return out
}

func TestFuseScoresAreInZeroExclusiveToOne(t *testing.T) {
	hits := domain.Fuse(cands([]string{"a", "b"}), cands([]string{"a", "c"}), 10)
	if hits[0].Ref != "a" || hits[0].Score != 1 {
		t.Fatalf("first in both lists must score exactly 1: %+v", hits[0])
	}
	for _, h := range hits {
		if h.Score <= 0 || h.Score > 1 {
			t.Fatalf("score out of range: %+v", h)
		}
	}
	if hits := domain.Fuse(cands([]string{"a"}), nil, 10); hits[0].Score != 0.5 {
		t.Fatalf("text-only first hit must score exactly 0.5: %+v", hits[0])
	}
}

func TestFuseScoreDoesNotDependOnTheVectorListBeingPresent(t *testing.T) {
	text := domain.Fuse(cands([]string{"a", "b"}), nil, 10)
	empty := domain.Fuse(cands([]string{"a", "b"}), []domain.Candidate{}, 10)
	if !reflect.DeepEqual(text, empty) {
		t.Fatalf("%v != %v", text, empty)
	}
}

func TestFuseAppliesLimitAfterFusion(t *testing.T) {
	hits := domain.Fuse(cands([]string{"a", "b", "c"}), cands([]string{"c", "b"}), 2)
	if len(hits) != 2 || hits[0].Ref != "c" || hits[1].Ref != "b" {
		// c: 1/63+1/61 > b: 1/62+1/62 > a: 1/61
		t.Fatalf("got %+v", hits)
	}
	if got := domain.Fuse(cands([]string{"a"}), nil, 0); len(got) != 0 {
		t.Fatalf("limit 0: %v", got)
	}
}

func TestFuseTakesTheServerFromTheRowNotFromTheRef(t *testing.T) {
	fts := []domain.Candidate{{Ref: "gitea.issues.create", Server: "gitea"}}
	hits := domain.Fuse(fts, nil, 5)
	if hits[0].Server != "gitea" || hits[0].Ref != "gitea.issues.create" {
		t.Fatalf("%+v", hits[0])
	}
	// a ref in both lists: the vector row's server wins (a later entry overwrites in the map)
	hits = domain.Fuse([]domain.Candidate{{Ref: "r", Server: "fromFTS"}}, []domain.Candidate{{Ref: "r", Server: "fromVector"}}, 5)
	if hits[0].Server != "fromVector" {
		t.Fatalf("%+v", hits[0])
	}
}

func TestFuseOrdersTiesTextListFirstThenVectorOnly(t *testing.T) {
	hits := domain.Fuse(cands([]string{"a"}), cands([]string{"b"}), 5)
	if hits[0].Ref != "a" || hits[1].Ref != "b" {
		t.Fatalf("%+v", hits)
	}
}

func TestFuseOfNothingIsEmpty(t *testing.T) {
	if got := domain.Fuse(nil, nil, 5); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}
