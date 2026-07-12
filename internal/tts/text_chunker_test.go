package tts

import "testing"

func TestTextChunkerBatchesTokenDeltas(t *testing.T) {
	chunker := NewTextChunker(6)
	var got []string
	for _, delta := range []string{"北", "京", "有", "故", "宫", "，", "也", "有", "长", "城", "和", "颐", "和", "园"} {
		got = append(got, chunker.Push(delta)...)
	}
	got = append(got, chunker.Flush())
	want := []string{"北京有故宫，", "也有长城和颐", "和园"}
	if len(got) != len(want) {
		t.Fatalf("chunks = %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chunks = %q", got)
		}
	}
}
