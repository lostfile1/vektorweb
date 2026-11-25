package luavm

import (
	"context"
	"fmt"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"
)

type Action struct {
	Type     string
	Text     string
	WorldURL string
	Pos      [3]float32
}

type WorldVM struct {
	mu      sync.Mutex
	L       *lua.LState
	actions []Action
	hudText string
	playerX float64
	playerY float64
	playerZ float64
}

func NewSandbox(script string) (*WorldVM, error) {
	L := lua.NewState(lua.Options{
		SkipOpenLibs:  true,
		CallStackSize: 128,
		RegistrySize:  512,
	})

	for _, pair := range []struct {
		name string
		fn   lua.LGFunction
	}{
		{lua.LoadLibName, lua.OpenBase},
		{lua.TabLibName, lua.OpenTable},
		{lua.StringLibName, lua.OpenString},
		{lua.MathLibName, lua.OpenMath},
	} {
		L.Push(L.NewFunction(pair.fn))
		L.Push(lua.LString(pair.name))
		L.Call(1, 0)
	}

	for _, dangerous := range []string{"dofile", "loadfile", "load", "loadstring", "require", "module"} {
		L.SetGlobal(dangerous, lua.LNil)
	}

	vm := &WorldVM{L: L}

	worldTbl := L.NewTable()
	L.SetFuncs(worldTbl, map[string]lua.LGFunction{
		"notify": func(L *lua.LState) int {
			msg := L.CheckString(1)
			if len(msg) > 200 {
				msg = msg[:200]
			}
			vm.actions = append(vm.actions, Action{Type: "NOTIFY", Text: msg})
			return 0
		},
		"set_hud": func(L *lua.LState) int {
			msg := L.CheckString(1)
			if len(msg) > 160 {
				msg = msg[:160]
			}
			vm.hudText = msg
			return 0
		},
		"teleport": func(L *lua.LState) int {
			x := float32(L.CheckNumber(1))
			y := float32(L.CheckNumber(2))
			z := float32(L.CheckNumber(3))
			vm.actions = append(vm.actions, Action{Type: "TELEPORT", Pos: [3]float32{x, y, z}})
			return 0
		},
		"portal": func(L *lua.LState) int {
			u := L.CheckString(1)
			if len(u) <= 512 {
				vm.actions = append(vm.actions, Action{Type: "PORTAL", WorldURL: u})
			}
			return 0
		},
		"player_pos": func(L *lua.LState) int {
			L.Push(lua.LNumber(vm.playerX))
			L.Push(lua.LNumber(vm.playerY))
			L.Push(lua.LNumber(vm.playerZ))
			return 3
		},
	})
	L.SetGlobal("world", worldTbl)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	L.SetContext(ctx)

	if err := L.DoString(script); err != nil {
		L.Close()
		return nil, fmt.Errorf("world.lua load error: %w", err)
	}
	L.SetContext(context.Background())
	return vm, nil
}

func (vm *WorldVM) Tick(dt float64, px, py, pz float32) ([]Action, string) {
	if vm == nil || vm.L == nil {
		return nil, ""
	}
	vm.mu.Lock()
	defer vm.mu.Unlock()

	vm.playerX = float64(px)
	vm.playerY = float64(py)
	vm.playerZ = float64(pz)
	vm.actions = nil

	fn := vm.L.GetGlobal("on_tick")
	if fn.Type() == lua.LTFunction {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
		vm.L.SetContext(ctx)
		_ = vm.L.CallByParam(lua.P{
			Fn:      fn,
			NRet:    0,
			Protect: true,
		}, lua.LNumber(dt))
		cancel()
		vm.L.SetContext(context.Background())
	}

	out := append([]Action(nil), vm.actions...)
	return out, vm.hudText
}

func (vm *WorldVM) Close() {
	if vm != nil && vm.L != nil {
		vm.mu.Lock()
		defer vm.mu.Unlock()
		vm.L.Close()
		vm.L = nil
	}
}
