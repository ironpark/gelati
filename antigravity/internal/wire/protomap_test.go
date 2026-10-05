package wire

import (
	"reflect"
	"testing"
)

func TestProtoMap(t *testing.T) {
	got, err := ProtoMap(&ActionListDirectory{
		DirectoryPath: new("file:///tmp"),
		Results: []*ActionListDirectoryResult{
			{Name: new("a.txt"), FileSize: new(Uint64(100))},
			{Name: new("sub"), IsDirectory: new(true)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"directory_path": "file:///tmp",
		"results": []any{
			map[string]any{"name": "a.txt", "file_size": "100"},
			map[string]any{"name": "sub", "is_directory": true},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ProtoMap = %#v\nwant %#v", got, want)
	}

	// Set zero values are kept; unset fields are omitted.
	got, err = ProtoMap(&ActionViewFile{FilePath: new(""), StartLine: new(uint32(0))})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"file_path": "", "start_line": float64(0)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ProtoMap = %#v, want %#v", got, want)
	}

	// Empty messages are empty maps; nil messages are nil.
	if got, _ := ProtoMap(&ActionInvokeSubagent{}); got == nil || len(got) != 0 {
		t.Fatalf("empty message = %#v", got)
	}
	if got, _ := ProtoMap((*ActionInvokeSubagent)(nil)); got != nil {
		t.Fatalf("nil message = %#v", got)
	}
}
