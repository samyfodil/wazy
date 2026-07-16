package instance

import "testing"

// Direct unit tests for the create/remove/rename directory + link helpers'
// logic and error branches (the real_fsops guest covers the happy path
// end-to-end; these pin the branches a single guest run does not reach,
// especially fsRename's directory-subtree move).

func TestFSDirCreate(t *testing.T) {
	fs := newWasiFS(map[string][]byte{"/file": []byte("x")})
	if code := fs.fsDirCreate("/d"); code != nil {
		t.Fatalf("create /d: %v", *code)
	}
	if !fs.fsIsDir("/d") {
		t.Fatal("/d should be a dir after create")
	}
	// creating over an existing dir -> exist
	if code := fs.fsDirCreate("/d"); code == nil || *code != uint32(wasiErrorCodeExist) {
		t.Fatalf("re-create /d: want exist, got %v", code)
	}
	// creating over an existing file -> exist
	if code := fs.fsDirCreate("/file"); code == nil || *code != uint32(wasiErrorCodeExist) {
		t.Fatalf("create over file: want exist, got %v", code)
	}
}

func TestFSDirRemove(t *testing.T) {
	fs := newWasiFS(map[string][]byte{"/d/inner": []byte("x")})
	// non-directory
	if code := fs.fsDirRemove("/nope"); code == nil || *code != uint32(wasiErrorCodeNotDirectory) {
		t.Fatalf("remove /nope: want not-directory, got %v", code)
	}
	// non-empty (has a file under it)
	if code := fs.fsDirRemove("/d"); code == nil || *code != uint32(wasiErrorCodeNotEmpty) {
		t.Fatalf("remove non-empty /d: want not-empty, got %v", code)
	}
	// empty explicit dir removes cleanly
	fs.fsDirCreate("/e")
	if code := fs.fsDirRemove("/e"); code != nil {
		t.Fatalf("remove empty /e: %v", *code)
	}
	if fs.fsIsDir("/e") {
		t.Fatal("/e should be gone")
	}
}

func TestFSRenameFile(t *testing.T) {
	fs := newWasiFS(map[string][]byte{"/a": []byte("A"), "/b": []byte("B")})
	// rename onto an existing path -> exist
	if code := fs.fsRename("/a", "/b"); code == nil || *code != uint32(wasiErrorCodeExist) {
		t.Fatalf("rename /a -> existing /b: want exist, got %v", code)
	}
	// nonexistent source -> no-entry
	if code := fs.fsRename("/missing", "/x"); code == nil || *code != uint32(wasiErrorCodeNoEntry) {
		t.Fatalf("rename missing: want no-entry, got %v", code)
	}
	// same path is a no-op success
	if code := fs.fsRename("/a", "/a"); code != nil {
		t.Fatalf("rename /a -> /a: %v", *code)
	}
	// real move
	if code := fs.fsRename("/a", "/c"); code != nil {
		t.Fatalf("rename /a -> /c: %v", *code)
	}
	if _, ok := fs.fsFileGet("/a"); ok {
		t.Fatal("/a should be gone after rename")
	}
	if v, _ := fs.fsFileGet("/c"); string(v) != "A" {
		t.Fatalf("/c = %q, want A", v)
	}
}

func TestFSRenameDirSubtree(t *testing.T) {
	fs := newWasiFS(map[string][]byte{
		"/src/f1":      []byte("1"),
		"/src/deep/f2": []byte("2"),
		"/src/deep/f3": []byte("3"),
		"/other":       []byte("o"),
	})
	fs.fsDirCreate("/src/emptydir") // explicit empty subdir moves too
	if code := fs.fsRename("/src", "/dst"); code != nil {
		t.Fatalf("rename dir subtree: %v", *code)
	}
	// every file re-keyed
	for old := range map[string]bool{"/src/f1": true, "/src/deep/f2": true, "/src/deep/f3": true} {
		if _, ok := fs.fsFileGet(old); ok {
			t.Fatalf("%s should be gone after subtree move", old)
		}
	}
	if v, _ := fs.fsFileGet("/dst/f1"); string(v) != "1" {
		t.Fatalf("/dst/f1 = %q, want 1", v)
	}
	if v, _ := fs.fsFileGet("/dst/deep/f2"); string(v) != "2" {
		t.Fatalf("/dst/deep/f2 = %q, want 2", v)
	}
	if !fs.fsIsDir("/dst/emptydir") {
		t.Fatal("/dst/emptydir should exist after subtree move")
	}
	if fs.fsIsDir("/src") {
		t.Fatal("/src should be gone after subtree move")
	}
	// unrelated file untouched
	if v, _ := fs.fsFileGet("/other"); string(v) != "o" {
		t.Fatalf("/other = %q, want o", v)
	}
	// renaming a dir onto an existing name -> exist
	if code := fs.fsRename("/dst", "/other"); code == nil || *code != uint32(wasiErrorCodeExist) {
		t.Fatalf("rename dir onto file: want exist, got %v", code)
	}
}
