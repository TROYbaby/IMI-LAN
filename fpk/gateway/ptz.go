package main

// 云台（PTZ）控制。
//
// 米家摄像机的转动走 MIoT 云端 miotspec/action，不需要额外的设备 token：
// 网关已经持有扫码拿到的 serviceToken + ssecurity，cloudRequest 能直接签名调用。
//
// 各型号云台动作的 siid/aiid/入参 piid 与取值命名并不统一（有的用 "left"，
// 有的用 "pan_left"，也有用数字枚举的），所以这里先去 miot-spec.org 按型号
// 拉一次公开规格，把动作和取值解析出来；解析不到再回退到最常见的 2/1/1。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	miotSpecListURL = "https://miot-spec.org/miot-spec-v2/instances?status=all"
	miotSpecTypeURL = "https://miot-spec.org/miot-spec-v2/instance?type="
)

// 前端只发语义方向，具体下发值由规格里的 value-list 匹配出来。
var ptzDirAliases = map[string][]string{
	"left":       {"left", "pan_left", "turn_left", "leftward", "左", "左转"},
	"right":      {"right", "pan_right", "turn_right", "rightward", "右", "右转"},
	"up":         {"up", "tilt_up", "turn_up", "upward", "上", "上转"},
	"down":       {"down", "tilt_down", "turn_down", "downward", "下", "下转"},
	"left_up":    {"left_up", "left-up", "up_left", "up-left", "左上"},
	"right_up":   {"right_up", "right-up", "up_right", "up-right", "右上"},
	"left_down":  {"left_down", "left-down", "down_left", "down-left", "左下"},
	"right_down": {"right_down", "right-down", "down_right", "down-right", "右下"},
}

// 方向展示顺序：前四个是必需项，斜向只在规格里确实有对应取值时才暴露。
var ptzDirOrder = []string{"left", "right", "up", "down", "left_up", "right_up", "left_down", "right_down"}

type ptzSpec struct {
	Model   string         `json:"model"`
	Siid    int            `json:"siid"`
	Aiid    int            `json:"aiid"`
	Piid    int            `json:"piid"`
	Format  string         `json:"format"`
	Action  string         `json:"action"`
	Source  string         `json:"source"`
	Note    string         `json:"note,omitempty"`
	Dirs    []string       `json:"dirs"`
	DirVals map[string]any `json:"-"`
}

type miotParam struct {
	Piid      int    `json:"piid"`
	Type      string `json:"type"`
	Desc      string `json:"description"`
	Format    string `json:"format"`
	ValueList []any  `json:"value-list"`
}

type miotAction struct {
	IID  int         `json:"iid"`
	Type string      `json:"type"`
	Desc string      `json:"description"`
	In   []miotParam `json:"in"`
}

type miotService struct {
	IID     int          `json:"iid"`
	Type    string       `json:"type"`
	Desc    string       `json:"description"`
	Actions []miotAction `json:"actions"`
}

type miotInstanceDetail struct {
	Services []miotService `json:"services"`
}

// 规格解析结果按型号缓存；同一个网关进程只拉一次公开规格表。
var (
	ptzMu        sync.Mutex
	ptzSpecCache = map[string]*ptzSpec{}
	miotIndex    map[string]string
	miotIndexAt  time.Time
)

func httpJSON(rawurl string, out any) error {
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "imi-lan-gateway/1.0 (+ptz)")
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

// modelIndex 拉一次全量型号表，cache 一天。表大约 1.5MB，只在本机内存里留映射。
func modelIndex() (map[string]string, error) {
	ptzMu.Lock()
	if miotIndex != nil && time.Since(miotIndexAt) < 24*time.Hour {
		idx := miotIndex
		ptzMu.Unlock()
		return idx, nil
	}
	ptzMu.Unlock()

	var list []struct {
		Type  string `json:"type"`
		Model string `json:"model"`
	}
	if err := httpJSON(miotSpecListURL, &list); err != nil {
		return nil, err
	}
	idx := make(map[string]string, len(list))
	for _, it := range list {
		if it.Model != "" && it.Type != "" {
			idx[strings.ToLower(it.Model)] = it.Type
		}
	}
	ptzMu.Lock()
	miotIndex, miotIndexAt = idx, time.Now()
	ptzMu.Unlock()
	return idx, nil
}

func ptzLooksLikeAction(a miotAction) bool {
	t := strings.ToLower(a.Type)
	d := a.Desc
	if strings.Contains(t, "ptz") || strings.Contains(t, "motor") {
		return true
	}
	if strings.Contains(t, "move") && !strings.Contains(t, "move-") {
		return true
	}
	for _, kw := range []string{"云台", "转动", "转向", "方向"} {
		if strings.Contains(d, kw) {
			return true
		}
	}
	return false
}

// 从规格里挑最像云台的那个动作：优先 type 同时含 ptz 和 move。
func pickPtzAction(svc miotService) (miotAction, int) {
	best := miotAction{}
	score := -1
	for _, a := range svc.Actions {
		if len(a.In) == 0 || !ptzLooksLikeAction(a) {
			continue
		}
		t := strings.ToLower(a.Type)
		c := 1
		if strings.Contains(t, "ptz") {
			c += 4
		}
		if strings.Contains(t, "move") {
			c += 2
		}
		if c > score {
			best, score = a, c
		}
	}
	return best, score
}

func matchValue(values []any, dir string) (any, bool) {
	alts := ptzDirAliases[dir]
	for _, alt := range alts {
		for _, v := range values {
			s, ok := v.(string)
			if !ok {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(s), alt) {
				return s, true
			}
		}
	}
	return nil, false
}

// resolvePtzSpec 解析某型号的云台动作；失败时返回内置回退（并带上 note 说明）。
func resolvePtzSpec(model string) *ptzSpec {
	key := strings.ToLower(model)
	ptzMu.Lock()
	if sp, ok := ptzSpecCache[key]; ok {
		ptzMu.Unlock()
		return sp
	}
	ptzMu.Unlock()

	spec := &ptzSpec{
		Model: model, Siid: 2, Aiid: 1, Piid: 1, Format: "string",
		Source: "fallback", Action: "builtin ptz-move",
		Note: "未能从 miot-spec.org 解析该型号，按最常见的 siid2/aiid1 尝试",
	}
	fallbackDirs := []string{"left", "right", "up", "down"}
	spec.Dirs, spec.DirVals = fallbackDirs, nil
	for _, d := range fallbackDirs {
		if v, ok := matchValue([]any{"left", "right", "up", "down", "left_up", "right_up", "left_down", "right_down"}, d); ok {
			if spec.DirVals == nil {
				spec.DirVals = map[string]any{}
			}
			spec.DirVals[d] = v
		}
	}

	if model != "" {
		if idx, err := modelIndex(); err == nil {
			if urn, ok := idx[key]; ok {
				var detail miotInstanceDetail
				if err := httpJSON(miotSpecTypeURL+url.QueryEscape(urn), &detail); err == nil {
					for _, svc := range detail.Services {
						act, score := pickPtzAction(svc)
						if score < 0 {
							continue
						}
						p := act.In[0]
						dirs := []string{}
						vals := map[string]any{}
						for _, d := range ptzDirOrder {
							if v, ok := matchValue(p.ValueList, d); ok {
								dirs = append(dirs, d)
								vals[d] = v
							}
						}
						if len(dirs) == 0 {
							spec.Note = "已找到云台动作，但入参取值无法识别（可能是数字枚举）"
							continue
						}
						spec.Siid, spec.Aiid, spec.Piid = svc.IID, act.IID, p.Piid
						spec.Format = p.Format
						spec.Action = act.Desc
						spec.Source = "spec"
						spec.Note = ""
						spec.Dirs, spec.DirVals = dirs, vals
						break
					}
				}
			}
		}
	}

	ptzMu.Lock()
	ptzSpecCache[key] = spec
	ptzMu.Unlock()
	return spec
}

// deviceModelOf 从 /rooms 缓存里查 did 对应的型号；缓存过期会自动重拉。
func (s *qrService) deviceModelOf(user, region, did string) (string, error) {
	data, err := s.fetchRooms(user, region)
	if err != nil {
		return "", err
	}
	for _, d := range data.Devices {
		if d.DID == did {
			return d.Model, nil
		}
	}
	return "", errors.New("房间里没有这个设备，可能已离线或不在当前账号")
}

func (s *qrService) ptzAuth(user, region string) (*cloudAuth, error) {
	if user == "" {
		return nil, errors.New("缺少账号")
	}
	if auth := s.authFromCredentials(user); auth != nil {
		return auth, nil
	}
	auth, _, err := s.getAuth(user, s.readXiaomiTokens()[user], false)
	if err != nil {
		return nil, err
	}
	if auth == nil {
		return nil, errors.New("小米账号登录失败，请重新扫码")
	}
	return auth, nil
}

// ---------------------------------------------------------------- HTTP 路由

func (s *qrService) registerPTZ(mux *http.ServeMux) {
	// 查询某个摄像头是否支持云台、支持哪些方向（前端据此决定要不要显示方向盘）。
	mux.HandleFunc("/ptz/spec", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "OPTIONS" {
			s.cors(w, r)
			w.WriteHeader(204)
			return
		}
		did := r.URL.Query().Get("did")
		region := r.URL.Query().Get("region")
		if region == "" {
			region = "cn"
		}
		user := r.URL.Query().Get("id")
		if user == "" {
			for k := range s.readXiaomiTokens() {
				user = k
				break
			}
		}
		if did == "" {
			s.sendJSON(w, r, 400, map[string]any{"ok": false, "error": "缺少 did"})
			return
		}
		model, err := s.deviceModelOf(user, region, did)
		if err != nil {
			s.sendJSON(w, r, 502, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		spec := resolvePtzSpec(model)
		s.sendJSON(w, r, 200, map[string]any{
			"ok": true, "did": did, "model": model, "spec": spec,
		})
	})

	mux.HandleFunc("/ptz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "OPTIONS" {
			s.cors(w, r)
			w.WriteHeader(204)
			return
		}
		q := r.URL.Query()
		did := q.Get("did")
		move := strings.ToLower(q.Get("move"))
		region := q.Get("region")
		if region == "" {
			region = "cn"
		}
		user := q.Get("id")
		if user == "" {
			for k := range s.readXiaomiTokens() {
				user = k
				break
			}
		}
		if did == "" || move == "" {
			s.sendJSON(w, r, 400, map[string]any{"ok": false, "error": "缺少 did 或 move"})
			return
		}

		model, err := s.deviceModelOf(user, region, did)
		if err != nil {
			s.sendJSON(w, r, 502, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		spec := resolvePtzSpec(model)

		// 允许显式覆盖，方便换型号时不用改代码就能试。
		siid, aiid, piid := spec.Siid, spec.Aiid, spec.Piid
		if v := q.Get("siid"); v != "" {
			fmt.Sscanf(v, "%d", &siid)
		}
		if v := q.Get("aiid"); v != "" {
			fmt.Sscanf(v, "%d", &aiid)
		}
		if v := q.Get("piid"); v != "" {
			fmt.Sscanf(v, "%d", &piid)
		}

		var value any
		if raw := q.Get("value"); raw != "" {
			value = raw
		} else if v, ok := spec.DirVals[move]; ok {
			value = v
		} else {
			s.sendJSON(w, r, 400, map[string]any{
				"ok": false, "error": "该型号没有方向：" + move,
				"dirs": spec.Dirs, "source": spec.Source, "note": spec.Note,
			})
			return
		}

		auth, err := s.ptzAuth(user, region)
		if err != nil {
			s.sendJSON(w, r, 502, map[string]any{"ok": false, "error": err.Error()})
			return
		}

		body := map[string]any{"params": []any{map[string]any{
			"did": did, "siid": siid, "aiid": aiid,
			"in": []any{map[string]any{"piid": piid, "value": value}},
		}}}
		res, err := cloudRequest(auth, region, "/miotspec/action", body)
		if err != nil {
			s.infof("ptz %s failed (model=%s siid=%d aiid=%d piid=%d value=%v): %v",
				move, model, siid, aiid, piid, value, err)
			s.sendJSON(w, r, 502, map[string]any{
				"ok": false, "error": err.Error(),
				"model": model, "siid": siid, "aiid": aiid, "piid": piid, "value": value,
			})
			return
		}
		s.sendJSON(w, r, 200, map[string]any{
			"ok": true, "model": model, "move": move, "value": value,
			"siid": siid, "aiid": aiid, "piid": piid, "result": res,
		})
	})
}
