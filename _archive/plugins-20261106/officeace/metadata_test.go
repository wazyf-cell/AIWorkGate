package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContextFromLabel(t *testing.T) {
	cases := map[string]int{"1M": 1048576, "198K": 202752, "128K": 131072, "512k": 524288}
	for label, want := range cases {
		got, ok := contextFromLabel(label)
		if !ok || got != want {
			t.Fatalf("contextFromLabel(%q) = %d,%v want %d", label, got, ok, want)
		}
	}
	for _, label := range []string{"Serverless", "Function Call", "1.5M", "M"} {
		if _, ok := contextFromLabel(label); ok {
			t.Fatalf("contextFromLabel(%q) should be unknown", label)
		}
	}
}

func TestLoadStaticCatalog(t *testing.T) {
	home := t.TempDir()
	res := filepath.Join(home, "resources")
	if err := os.MkdirAll(res, 0o755); err != nil {
		t.Fatal(err)
	}
	js := `const d={"glm-5.2":{name:"GLM-5.2",description:` + "`GLM-5.2 旗舰`" +
		`,labels:["文本生成","深度思考","1M"],developer:"智谱",icon:"/x.svg"},` +
		`"kimi-k2.6":{name:"Kimi-K2.6",description:"Kimi",labels:["文本生成","图像理解","256K"],developer:"Kimi"}}`
	if err := os.WriteFile(filepath.Join(res, "app.asar"), []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog := loadStaticCatalog(home)
	glm, ok := catalog["glm-5.2"]
	if !ok || glm.Name != "GLM-5.2" || glm.Vendor != "智谱" || len(glm.Labels) != 3 {
		t.Fatalf("glm = %+v (catalog %d)", glm, len(catalog))
	}
	if kimi := catalog["kimi-k2.6"]; kimi.Name != "Kimi-K2.6" || kimi.Vendor != "Kimi" {
		t.Fatalf("kimi = %+v", kimi)
	}
	if got := loadStaticCatalog(filepath.Join(home, "missing")); got != nil {
		t.Fatalf("missing bundle should yield nil, got %+v", got)
	}
}

func TestApplyStaticMetadata(t *testing.T) {
	known := boolPtr(false)
	models := []Model{
		{ID: "glm-5.2"},
		{ID: "kimi-k2.6", Name: "Live Name", ContextLength: 1000, Vision: known},
	}
	meta := map[string]staticMeta{
		"glm-5.2":    {Name: "GLM-5.2", Vendor: "智谱", Labels: []string{"深度思考", "1M"}},
		"kimi-k2.6":  {Name: "Kimi-K2.6", Vendor: "Kimi", Labels: []string{"图像理解", "256K"}},
		"absent-one": {Name: "Absent"},
	}
	out := applyStaticMetadata(models, meta)
	glm := out[0]
	if glm.Name != "GLM-5.2" || glm.Vendor != "智谱" || glm.ContextLength != 1048576 ||
		len(glm.Thinking) != 2 || glm.Vision != nil {
		t.Fatalf("glm = %+v", glm)
	}
	kimi := out[1]
	if kimi.Name != "Live Name" || kimi.ContextLength != 1000 || kimi.Vision == nil || *kimi.Vision {
		t.Fatalf("live values must win: %+v", kimi)
	}
}
