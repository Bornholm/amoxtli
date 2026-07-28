package blob

import "testing"

func TestURIRoundtrip(t *testing.T) {
	hash := ComputeHash([]byte("content"))

	uri := URI(hash)

	if e, g := "amoxtli://images/"+string(hash), uri; e != g {
		t.Errorf("URI: expected %q, got %q", e, g)
	}

	parsed, ok := ParseURI(uri)
	if !ok {
		t.Fatalf("ParseURI(%q): expected it to parse", uri)
	}

	if e, g := hash, parsed; e != g {
		t.Errorf("ParseURI: expected %q, got %q", e, g)
	}
}

func TestParseURI(t *testing.T) {
	hash := ComputeHash([]byte("content"))

	testCases := []struct {
		Name     string
		Input    string
		Expected Hash
		OK       bool
	}{
		{"full uri", URI(hash), hash, true},
		{"bare hash", string(hash), hash, true},
		{"surrounding spaces", "  " + URI(hash) + "\n", hash, true},
		{"empty", "", "", false},
		{"not a hash", "amoxtli://images/not-a-hash", "", false},
		{"truncated hash", string(hash)[:10], "", false},
		{"path traversal", "amoxtli://images/../../etc/passwd", "", false},
		{"other scheme", "https://example.net/" + string(hash), "", false},
	}

	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			parsed, ok := ParseURI(tc.Input)

			if tc.OK != ok {
				t.Fatalf("ParseURI(%q): expected ok=%v, got %v", tc.Input, tc.OK, ok)
			}

			if tc.Expected != parsed {
				t.Errorf("ParseURI(%q): expected %q, got %q", tc.Input, tc.Expected, parsed)
			}
		})
	}
}

func TestHashValid(t *testing.T) {
	if !ComputeHash([]byte("content")).Valid() {
		t.Error("a computed hash must be valid")
	}

	for _, invalid := range []Hash{"", "abc", "../../etc/passwd", "zz" + Hash(string(make([]byte, 0)))} {
		if invalid.Valid() {
			t.Errorf("Hash(%q).Valid(): expected false", invalid)
		}
	}
}

func TestCheckPut(t *testing.T) {
	if _, err := CheckPut("image/png", nil, 0); err == nil {
		t.Error("CheckPut with empty content: expected an error")
	}

	if _, err := CheckPut("", []byte("x"), 0); err == nil {
		t.Error("CheckPut without a mime type: expected an error")
	}

	if _, err := CheckPut("image/png", []byte("xx"), 1); err == nil {
		t.Error("CheckPut oversized: expected an error")
	}

	hash, err := CheckPut("image/png", []byte("content"), 0)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if e, g := ComputeHash([]byte("content")), hash; e != g {
		t.Errorf("CheckPut hash: expected %q, got %q", e, g)
	}
}

func TestScanHashes(t *testing.T) {
	first := ComputeHash([]byte("first"))
	second := ComputeHash([]byte("second"))

	content := []byte("# Titre\n\n![A](" + URI(first) + ")\n\nDu texte ![B](" + URI(second) + ") et ![A encore](" + URI(first) + ").\n")

	hashes := ScanHashes(content)

	if e, g := 3, len(hashes); e != g {
		t.Fatalf("ScanHashes: expected %d hashes, got %d (%v)", e, g, hashes)
	}

	if hashes[0] != first || hashes[1] != second || hashes[2] != first {
		t.Errorf("ScanHashes: expected [%s %s %s], got %v", first, second, first, hashes)
	}
}

func TestScanHashesIgnoresGarbage(t *testing.T) {
	testCases := []string{
		"",
		"no reference at all",
		"amoxtli://images/",
		"amoxtli://images/tooshort)",
		"amoxtli://images/../../etc/passwd",
		"https://example.net/" + string(ComputeHash([]byte("x"))),
	}

	for _, content := range testCases {
		if hashes := ScanHashes([]byte(content)); len(hashes) != 0 {
			t.Errorf("ScanHashes(%q): expected none, got %v", content, hashes)
		}
	}
}
