// 一時的な調査ツール: Dart VM service に任意の RPC を投げて所要時間と結果を見る。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type resp struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	} `json:"error"`
}

func main() {
	ws := flag.String("ws", "", "ws://127.0.0.1:PORT/ws")
	method := flag.String("method", "", "RPC method to call")
	paramsJSON := flag.String("params", "{}", "params as JSON; isolateId is filled in automatically")
	listExt := flag.Bool("list-ext", false, "list the isolate's registered service extensions")
	head := flag.Int("head", 900, "how many bytes of the result to print")
	flag.Parse()

	c, _, err := websocket.DefaultDialer.Dial(*ws, nil)
	must(err)
	defer func() { _ = c.Close() }()

	seq := 0
	call := func(m string, params map[string]any) (json.RawMessage, time.Duration, error) {
		seq++
		req := map[string]any{"jsonrpc": "2.0", "id": seq, "method": m}
		if params != nil {
			req["params"] = params
		}
		b, _ := json.Marshal(req)
		t0 := time.Now()
		must(c.WriteMessage(websocket.TextMessage, b))
		for {
			_, msg, err := c.ReadMessage()
			must(err)
			var r resp
			if json.Unmarshal(msg, &r) != nil || r.ID != seq {
				continue // stream event, or someone else's answer
			}
			if r.Error != nil {
				return nil, time.Since(t0), fmt.Errorf("RPC %s error %d: %s\n%s", m, r.Error.Code, r.Error.Message, r.Error.Data)
			}
			return r.Result, time.Since(t0), nil
		}
	}

	var vm struct {
		Isolates []struct{ ID string } `json:"isolates"`
	}
	raw, _, err := call("getVM", nil)
	must(err)
	must(json.Unmarshal(raw, &vm))
	if len(vm.Isolates) == 0 {
		fail("no isolates")
	}
	iso := vm.Isolates[0].ID

	if *listExt {
		raw, _, err := call("getIsolate", map[string]any{"isolateId": iso})
		must(err)
		var isolate struct {
			ExtensionRPCs []string `json:"extensionRPCs"`
		}
		must(json.Unmarshal(raw, &isolate))
		fmt.Printf("registered service extensions: %d\n", len(isolate.ExtensionRPCs))
		for _, e := range isolate.ExtensionRPCs {
			fmt.Println(" ", e)
		}
		return
	}

	if *method == "" {
		fail("-method is required (or -list-ext)")
	}
	var params map[string]any
	must(json.Unmarshal([]byte(*paramsJSON), &params))
	if params == nil {
		params = map[string]any{}
	}
	if _, ok := params["isolateId"]; !ok {
		params["isolateId"] = iso
	}

	out, elapsed, err := call(*method, params)
	if err != nil {
		fmt.Printf("elapsed %s\n", elapsed)
		fail(err.Error())
	}
	fmt.Printf("elapsed %s / %d bytes\n", elapsed, len(out))
	fmt.Println(truncate(strings.TrimSpace(string(out)), *head))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "error:", msg)
	os.Exit(1)
}

func must(err error) {
	if err != nil {
		fail(err.Error())
	}
}
