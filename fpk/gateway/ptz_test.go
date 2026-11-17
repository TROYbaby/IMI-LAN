package main

import (
	"testing"
)

// 锁死这次的坑：miot-spec 的型号列表接口返回的是 {"instances":[...]}，
// 按裸数组解析会静默失败，导致所有型号都掉进 fallback、云台方向永远对不上。
func TestParseModelIndexWrapper(t *testing.T) {
	body := []byte(`{"instances":[
		{"status":"released","model":"Chuangmi.Camera.81AC1","type":"urn:miot-spec-v2:device:camera:0000A01C:chuangmi-81ac1:5"},
		{"status":"released","model":"xiaovv.camera.q8","type":"urn:miot-spec-v2:device:camera:0000A01C:xiaovv-q8:2"}
	]}`)

	idx, err := parseModelIndex(body)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(idx) != 2 {
		t.Fatalf("期望 2 条，实际 %d", len(idx))
	}
	// 型号统一转小写，云端返回的大小写不固定
	if got := idx["chuangmi.camera.81ac1"]; got != "urn:miot-spec-v2:device:camera:0000A01C:chuangmi-81ac1:5" {
		t.Fatalf("大小写归一化失败: %q", got)
	}
}

func TestParseModelIndexBareArrayFails(t *testing.T) {
	// 裸数组是以前错误的假设，必须报错而不是返回空表装成功
	if _, err := parseModelIndex([]byte(`[{"model":"a","type":"b"}]`)); err == nil {
		t.Fatal("裸数组应当返回错误，避免又静默退化成 fallback")
	}
}

// 摄像机型号的公开规格里没有云台动作：解析结果必须是「不支持」，
// 前端据此隐藏方向盘，而不是盲调云端让用户看到一堆报错。
func TestResolvePtzSpecNoCloudAction(t *testing.T) {
	if testing.Short() {
		t.Skip("需要访问 miot-spec.org")
	}
	if _, err := modelIndex(); err != nil {
		t.Skipf("外网不可用，跳过: %v", err)
	}

	for _, model := range []string{"chuangmi.camera.81ac1", "xiaovv.camera.q8"} {
		spec := resolvePtzSpec(model)
		if spec.Supported {
			t.Errorf("%s: 公开规格里没有云台动作，Supported 应为 false（来源=%s dirs=%v）",
				model, spec.Source, spec.Dirs)
		}
		if spec.PTZCapable && len(spec.Dirs) > 0 {
			t.Errorf("%s: 不支持却给出了方向列表", model)
		}
	}
}

func TestResolvePtzSpecC700IsPtzCamera(t *testing.T) {
	if testing.Short() {
		t.Skip("需要访问 miot-spec.org")
	}
	if _, err := modelIndex(); err != nil {
		t.Skipf("外网不可用，跳过: %v", err)
	}
	// C700 规格里有 ptz-camera 属性，属于云台机，但没有可下发的动作
	if spec := resolvePtzSpec("chuangmi.camera.81ac1"); !spec.PTZCapable {
		t.Fatalf("C700 应被识别为云台机，实际 ptzCapable=false（来源=%s note=%s）",
			spec.Source, spec.Note)
	}
}
