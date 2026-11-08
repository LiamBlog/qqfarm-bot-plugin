package main

import "testing"

func TestPickFarmCode(t *testing.T) {
	ok := []map[string]any{
		{"code": "ABC123"},
		{"farm_code": " X1 "},
		{"farmCode": "C2"},
		{"login_code": "D3"},
		{"authCode": "E4"},
	}
	for _, in := range ok {
		code, err := pickFarmCode(in)
		if err != nil || code == "" {
			t.Fatalf("应从 %v 取到 Code：code=%q err=%v", in, code, err)
		}
	}

	// 短数字疑似业务状态码，应跳过；优先取后续有效字段
	code, err := pickFarmCode(map[string]any{"code": 200, "farm_code": "VALID"})
	if err != nil || code != "VALID" {
		t.Fatalf("应跳过短数字：code=%q err=%v", code, err)
	}

	if _, err := pickFarmCode(map[string]any{"code": 200, "msg": "ok"}); err == nil {
		t.Fatal("无有效 Code 时应报错")
	}
}

func TestSamePanelURLs(t *testing.T) {
	a := []Panel{{BaseURL: "http://a"}, {BaseURL: "http://b"}}
	b := []Panel{{BaseURL: "http://a"}, {BaseURL: "http://c"}}
	if samePanelURLs(a, a) != true {
		t.Fatal("相同面板地址应返回 true")
	}
	if samePanelURLs(a, b) != false {
		t.Fatal("地址变化应返回 false")
	}
	if samePanelURLs(a, a[:1]) != false {
		t.Fatal("数量变化应返回 false")
	}
}
