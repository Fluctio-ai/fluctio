package setup

import "testing"

// TestParseTodoMarkdown locks the checkbox grammar the chat UI depends on:
// [ ] pending, [x]/[X] done, and the newer [-]/[~] cancelled. A regression
// here either hides the progress panel or leaves cancelled steps pinned as
// pending forever.
func TestParseTodoMarkdown(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []map[string]any
	}{
		{
			name: "pending and done",
			in:   "- [ ] alpha\n- [x] beta\n",
			want: []map[string]any{
				{"text": "alpha", "done": false},
				{"text": "beta", "done": true},
			},
		},
		{
			name: "cancelled markers",
			in:   "- [-] dropped\n- [~] also dropped\n- [ ] live\n",
			want: []map[string]any{
				{"text": "dropped", "done": false, "cancelled": true},
				{"text": "also dropped", "done": false, "cancelled": true},
				{"text": "live", "done": false},
			},
		},
		{
			name: "duplicate text merges, done and cancelled are sticky",
			in:   "- [ ] step\n- [x] step\n- [ ] other\n- [-] other\n",
			want: []map[string]any{
				{"text": "step", "done": true},
				{"text": "other", "done": false, "cancelled": true},
			},
		},
		{
			name: "asterisk bullets, non-checkbox lines ignored",
			in:   "# Plan\nsome prose\n* [x] built\n* not a checkbox\n",
			want: []map[string]any{
				{"text": "built", "done": true},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTodoMarkdown(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (got %v)", len(got), len(tc.want), got)
			}
			for i, w := range tc.want {
				for k, wv := range w {
					gv, ok := got[i][k]
					if !ok {
						t.Errorf("item %d missing key %q; got %v", i, k, got[i])
						continue
					}
					if gv != wv {
						t.Errorf("item %d key %q = %v, want %v", i, k, gv, wv)
					}
				}
			}
		})
	}
}
