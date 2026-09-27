package blueprint

import "testing"

func TestListModulePathsSkipsMissingFiles(t *testing.T) {
	ctx := NewContext()
	ctx.MockFileSystem(map[string][]byte{
		"keep/Android.bp": []byte("\n"),
		MockModuleListFile: []byte("keep/Android.bp\ndevice/xiaomi/jason/pocketmode/Android.bp\n"),
	})

	paths, err := ctx.ListModulePaths(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "keep/Android.bp" {
		t.Fatalf("paths = %#v", paths)
	}
}
