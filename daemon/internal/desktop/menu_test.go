package desktop

import (
	"strings"
	"testing"
)

func TestBuildMenuUsesStableApprovedLayout(t *testing.T) {
	menu := BuildMenu(MenuOptions{
		Snapshot:       Snapshot{State: StateOnline},
		Configured:     true,
		WorkspaceName:  "Workspace One",
		LaunchAtLogin:  true,
		DesktopVersion: " 1.2.3 ",
	})
	if err := menu.Validate(); err != nil {
		t.Fatalf("BuildMenu().Validate() error = %v", err)
	}

	wantIDs := []MenuItemID{
		MenuItemStatus,
		MenuItemOpenCodesk,
		MenuItemConnect,
		MenuItemRestart,
		MenuItemLaunchAtLogin,
		MenuItemOpenLogs,
		MenuItemVersion,
		MenuItemQuit,
	}
	if len(menu.Items) != len(wantIDs) {
		t.Fatalf("menu item count = %d, want %d", len(menu.Items), len(wantIDs))
	}
	for index, wantID := range wantIDs {
		if got := menu.Items[index].ID; got != wantID {
			t.Errorf("menu item %d ID = %q, want %q", index, got, wantID)
		}
	}
	for _, item := range menu.Items {
		if item.Title == "Start" || item.Title == "Stop" {
			t.Errorf("menu contains forbidden lifecycle command %q", item.Title)
		}
	}

	status := mustMenuItem(t, menu, MenuItemStatus)
	if status.Title != "Codesk - Workspace One - Connected" || status.Enabled {
		t.Errorf("status item = %+v", status)
	}
	launch := mustMenuItem(t, menu, MenuItemLaunchAtLogin)
	if !launch.Checkable || !launch.Checked || !launch.Enabled {
		t.Errorf("launch-at-login item = %+v", launch)
	}
	version := mustMenuItem(t, menu, MenuItemVersion)
	if version.Title != "Version 1.2.3" || version.Enabled {
		t.Errorf("version item = %+v", version)
	}
}

func TestBuildMenuStatusLabels(t *testing.T) {
	tests := []struct {
		state State
		want  string
	}{
		{StateStarting, "Codesk - Starting"},
		{StateOnline, "Codesk - Connected"},
		{StateRetrying, "Codesk - Reconnecting"},
		{StateReconnectRequired, "Codesk - Needs connection"},
		{StateQuitting, "Codesk - Quitting"},
		{State(99), "Codesk - Unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.state.String(), func(t *testing.T) {
			menu := BuildMenu(MenuOptions{Snapshot: Snapshot{State: tt.state}})
			if got := mustMenuItem(t, menu, MenuItemStatus).Title; got != tt.want {
				t.Errorf("status title = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildMenuConnectionActions(t *testing.T) {
	tests := []struct {
		name           string
		state          State
		configured     bool
		wantConnect    string
		connectEnabled bool
		openEnabled    bool
	}{
		{
			name:           "first connection",
			state:          StateStarting,
			wantConnect:    "Connect...",
			connectEnabled: true,
		},
		{
			name:        "configured and online",
			state:       StateOnline,
			configured:  true,
			wantConnect: "Connect...",
			openEnabled: true,
		},
		{
			name:        "configured and retrying",
			state:       StateRetrying,
			configured:  true,
			wantConnect: "Connect...",
			openEnabled: true,
		},
		{
			name:           "credentials rejected",
			state:          StateReconnectRequired,
			configured:     true,
			wantConnect:    "Reconnect...",
			connectEnabled: true,
			openEnabled:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			menu := BuildMenu(MenuOptions{
				Snapshot:   Snapshot{State: tt.state},
				Configured: tt.configured,
			})
			connect := mustMenuItem(t, menu, MenuItemConnect)
			if connect.Title != tt.wantConnect || connect.Enabled != tt.connectEnabled {
				t.Errorf("connect item = %+v, want title %q enabled %t", connect, tt.wantConnect, tt.connectEnabled)
			}
			if got := mustMenuItem(t, menu, MenuItemOpenCodesk).Enabled; got != tt.openEnabled {
				t.Errorf("Open Codesk enabled = %t, want %t", got, tt.openEnabled)
			}
		})
	}
}

func TestBuildMenuQuittingDisablesEveryAction(t *testing.T) {
	menu := BuildMenu(MenuOptions{
		Snapshot:      Snapshot{State: StateQuitting},
		Configured:    true,
		LaunchAtLogin: true,
	})
	for _, item := range menu.Items {
		if item.Action != MenuActionNone && item.Enabled {
			t.Errorf("action %q remains enabled while quitting", item.Action)
		}
	}
	if !mustMenuItem(t, menu, MenuItemLaunchAtLogin).Checked {
		t.Error("quitting model lost the current launch-at-login check state")
	}
}

func TestBuildMenuUsesUnknownVersionFallback(t *testing.T) {
	menu := BuildMenu(MenuOptions{})
	if got := mustMenuItem(t, menu, MenuItemVersion).Title; got != "Version unknown" {
		t.Errorf("version title = %q, want %q", got, "Version unknown")
	}
	status := mustMenuItem(t, BuildMenu(MenuOptions{WorkspaceName: "  Team  "}), MenuItemStatus)
	if !strings.Contains(status.Title, "Team") || strings.Contains(status.Title, "  Team  ") {
		t.Errorf("workspace status title was not trimmed: %q", status.Title)
	}
}

func TestMenuModelValidateRejectsContractDrift(t *testing.T) {
	valid := BuildMenu(MenuOptions{Snapshot: Snapshot{State: StateOnline}, Configured: true})
	mutate := func(apply func(*MenuModel)) MenuModel {
		model := MenuModel{Items: append([]MenuItem(nil), valid.Items...)}
		apply(&model)
		return model
	}
	tests := []struct {
		name  string
		model MenuModel
	}{
		{
			name: "missing item",
			model: mutate(func(model *MenuModel) {
				model.Items = model.Items[:len(model.Items)-1]
			}),
		},
		{
			name: "wrong order",
			model: mutate(func(model *MenuModel) {
				model.Items[0], model.Items[1] = model.Items[1], model.Items[0]
			}),
		},
		{
			name: "wrong action",
			model: mutate(func(model *MenuModel) {
				model.Items[1].Action = MenuActionQuit
			}),
		},
		{
			name: "empty title",
			model: mutate(func(model *MenuModel) {
				model.Items[1].Title = "  "
			}),
		},
		{
			name: "enabled status",
			model: mutate(func(model *MenuModel) {
				model.Items[0].Enabled = true
			}),
		},
		{
			name: "checked command",
			model: mutate(func(model *MenuModel) {
				model.Items[1].Checked = true
			}),
		},
		{
			name: "checkable command",
			model: mutate(func(model *MenuModel) {
				model.Items[1].Checkable = true
			}),
		},
		{
			name: "launch is not checkable",
			model: mutate(func(model *MenuModel) {
				model.Items[4].Checkable = false
			}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.model.Validate(); err == nil {
				t.Fatal("Validate() error = nil, want contract error")
			}
		})
	}
}

func TestMenuModelItemRejectsUnknownID(t *testing.T) {
	menu := BuildMenu(MenuOptions{})
	if _, err := menu.Item(MenuItemID("missing")); err == nil {
		t.Fatal("Item(missing) error = nil")
	}
}

func mustMenuItem(t *testing.T, menu MenuModel, id MenuItemID) MenuItem {
	t.Helper()
	item, err := menu.Item(id)
	if err != nil {
		t.Fatalf("menu.Item(%q) error = %v", id, err)
	}
	return item
}
