package memory

import "testing"

func TestSplitFrontmatterFlowTags(t *testing.T) {
	pm := SplitFrontmatter("---\ntitle: axis 架构\ntags: [axis, arch]\n---\n# body\n")
	if len(pm.Warnings) > 0 {
		t.Fatalf("unexpected warnings: %v", pm.Warnings)
	}
	if pm.Frontmatter["title"] != "axis 架构" {
		t.Errorf("title = %q", pm.Frontmatter["title"])
	}
	if pm.Frontmatter["tags"] == "" {
		t.Errorf("tags empty")
	}
	if pm.Body != "# body\n" {
		t.Errorf("body = %q", pm.Body)
	}
}
