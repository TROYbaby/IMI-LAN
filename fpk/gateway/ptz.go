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
	// Supported 只有从公开规格里真解析出云台动作才为 true。
	// 前端据此决定要不要显示方向盘 —— 盲调云端只会让用户以为坏了。
	Supported bool `json:"supported"`
	// PTZCapable 表示该型号本身是云台机（规格里有 ptz 相关属性），
	// 但当前引擎没有可用的控制通道。
	PTZCapable bool `json:"ptzCapable"`
}

type miotParam struct {
	// 属性用 IID 编号，动作入参用 Piid；in 里写数字时指的是同服务下属性的 IID。
	IID       int    `json:"iid"`
	Piid      int    `json:"piid"`
	Type      string `json:"type"`
	Desc      string `json:"description"`
	Format    string `json:"format"`
	ValueList []any  `json:"value-list"`
}

// 规格里 in 有两种写法：[{"piid":1,"value-list":[...]}] 和 [1]。
// 后者只给编号，取值要从同服务的同名属性上找。按对象硬解会让整个规格解析失败
// （这正是之前「云台怎么都不动」的真正原因：解析失败 → 全部退化成 fallback）。
func (m *miotParam) UnmarshalJSON(b []byte) error {
	var n int
	if err := json.Unmarshal(b, &n); err == nil {
		*m = miotParam{Piid: n}
		return nil
	}
	type raw miotParam
	var r raw
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	*m = miotParam(r)
	return nil
}

// propValues 按 piid 去同服务里找属性的取值表（in 只给编号时用）。
func propValues(svc miotService, piid int) ([]any, string) {
	for _, p := range svc.Properties {
		if p.IID == piid {
			return p.ValueList, p.Format
		}
	}
	return nil, ""
}

type miotAction struct {
	IID  int         `json:"iid"`
	Type string      `json:"type"`
	Desc string      `json:"description"`
	In   []miotParam `json:"in"`
}

type miotService struct {
	IID        int          `json:"iid"`
	Type       string       `json:"type"`
	Desc       string       `json:"description"`
	Actions    []miotAction `json:"actions"`
	Properties []miotParam  `json:"properties"`
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

func httpBytes(rawurl string) ([]byte, error) {
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "imi-lan-gateway/1.0 (+ptz)")
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, 16<<20))
}

func httpJSON(rawurl string, out any) error {
	body, err := httpBytes(rawurl)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

// parseModelIndex 解析型号 -> 规格 urn 的映射。
// 接口返回的是 {"instances":[...]}，不是裸数组；之前按数组解析会静默失败，
// 于是所有型号都掉进 fallback，云台方向永远对不上。
func parseModelIndex(body []byte) (map[string]string, error) {
	var payload struct {
		Instances []struct {
			Type  string `json:"type"`
			Model string `json:"model"`
		} `json:"instances"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if len(payload.Instances) == 0 {
		return nil, errors.New("规格列表为空：接口结构可能又变了")
	}
	idx := make(map[string]string, len(payload.Instances))
	for _, it := range payload.Instances {
		if it.Model != "" && it.Type != "" {
			idx[strings.ToLower(it.Model)] = it.Type
		}
	}
	return idx, nil
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

	body, err := httpBytes(miotSpecListURL)
	if err != nil {
		return nil, err
	}
	idx, err := parseModelIndex(body)
	if err != nil {
		return nil, err
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
					// 先看这台是不是云台机：规格里有 ptz-camera / ptz 系列属性
					for _, svc := range detail.Services {
						for _, p := range svc.Properties {
							t := strings.ToLower(p.Type + " " + p.Desc)
							if strings.Contains(t, "ptz") {
								spec.PTZCapable = true
							}
						}
					}
					foundAction := false
					for _, svc := range detail.Services {
						act, score := pickPtzAction(svc)
						if score < 0 {
							continue
						}
						foundAction = true
						p := act.In[0]
						// in 只给编号时，取值表挂在同服务的属性上
						choices := p.ValueList
						format := p.Format
						if len(choices) == 0 && p.Piid > 0 {
							if pv, pf := propValues(svc, p.Piid); len(pv) > 0 {
								choices, format = pv, pf
							}
						}
						dirs := []string{}
						vals := map[string]any{}
						for _, d := range ptzDirOrder {
							if v, ok := matchValue(choices, d); ok {
								dirs = append(dirs, d)
								vals[d] = v
							}
						}
						if len(dirs) == 0 {
							spec.Note = "已找到云台动作，但入参取值无法识别（可能是数字枚举）"
							continue
						}
						spec.Siid, spec.Aiid, spec.Piid = svc.IID, act.IID, p.Piid
						spec.Format = format
						spec.Action = act.Desc
						spec.Source = "spec"
						spec.Note = ""
						spec.Dirs, spec.DirVals = dirs, vals
						spec.Supported = true
						break
					}
					if !foundAction {
						// 这批摄像机（C700 / xiaovv / 叮零）就是这种情况：
						// 云台机，但转动命令走 P2P 视频连接里的 cmd 通道。
						spec.Note = "该型号的公开规格里没有云台动作，转动命令走 P2P 通道"
					}
				}
			}
		}
	}

	// 没解析出可用动作时不要保留 fallback 的方向表：
	// 否则接口会同时说「不支持」又给出一堆方向，前端和调试都容易误判。
	if !spec.Supported {
		spec.Dirs, spec.DirVals = nil, nil
		spec.Source = "unsupported"
		spec.Action = ""
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

		// 公开规格里没有云台动作时，云端 /miotspec/action 一定失败（这批摄像机
		// 的转动走的是 P2P 视频连接里的 cmd 通道，官方 go2rtc 不开放）。
		// 与其返回一个看不懂的云端错误，不如直接说明原因。
		// 显式带 siid/aiid/value 时仍然放行，方便换型号手动试。
		if !spec.Supported && q.Get("siid") == "" && q.Get("value") == "" {
			reason := "该型号的公开 MIoT 规格没有云台动作，云端控制通道不可用"
			if spec.PTZCapable {
				reason = "这台是云台机，但转动命令走 P2P 通道，当前播放引擎（go2rtc）不开放该通道"
			}
			s.sendJSON(w, r, 501, map[string]any{
				"ok": false, "error": reason, "model": model,
				"ptzCapable": spec.PTZCapable, "supported": false, "note": spec.Note,
			})
			return
		}

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
