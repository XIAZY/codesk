package desktop

import (
	"fmt"
	"strings"
)

type MenuItemID string

const (
	MenuItemStatus        MenuItemID = "status"
	MenuItemOpenCodesk    MenuItemID = "open-codesk"
	MenuItemConnect       MenuItemID = "connect"
	MenuItemRestart       MenuItemID = "restart"
	MenuItemLaunchAtLogin MenuItemID = "launch-at-login"
	MenuItemOpenLogs      MenuItemID = "open-logs"
	MenuItemVersion       MenuItemID = "version"
	MenuItemQuit          MenuItemID = "quit"
)

type MenuAction string

const (
	MenuActionNone                MenuAction = ""
	MenuActionOpenCodesk          MenuAction = "open-codesk"
	MenuActionConnect             MenuAction = "connect"
	MenuActionRestart             MenuAction = "restart"
	MenuActionToggleLaunchAtLogin MenuAction = "toggle-launch-at-login"
	MenuActionOpenLogs            MenuAction = "open-logs"
	MenuActionQuit                MenuAction = "quit"
)

type MenuItem struct {
	ID        MenuItemID
	Action    MenuAction
	Title     string
	Enabled   bool
	Checkable bool
	Checked   bool
}

type MenuModel struct {
	Items []MenuItem
}

type MenuOptions struct {
	Snapshot       Snapshot
	Configured     bool
	WorkspaceName  string
	LaunchAtLogin  bool
	DesktopVersion string
}

var menuLayout = []struct {
	id     MenuItemID
	action MenuAction
}{
	{MenuItemStatus, MenuActionNone},
	{MenuItemOpenCodesk, MenuActionOpenCodesk},
	{MenuItemConnect, MenuActionConnect},
	{MenuItemRestart, MenuActionRestart},
	{MenuItemLaunchAtLogin, MenuActionToggleLaunchAtLogin},
	{MenuItemOpenLogs, MenuActionOpenLogs},
	{MenuItemVersion, MenuActionNone},
	{MenuItemQuit, MenuActionQuit},
}

func BuildMenu(options MenuOptions) MenuModel {
	quitting := options.Snapshot.State == StateQuitting
	connectTitle := "Connect..."
	if options.Configured && options.Snapshot.State == StateReconnectRequired {
		connectTitle = "Reconnect..."
	}
	version := strings.TrimSpace(options.DesktopVersion)
	if version == "" {
		version = "unknown"
	}

	return MenuModel{Items: []MenuItem{
		{ID: MenuItemStatus, Title: statusTitle(options.Snapshot.State, options.WorkspaceName)},
		{
			ID:      MenuItemOpenCodesk,
			Action:  MenuActionOpenCodesk,
			Title:   "Open Codesk",
			Enabled: options.Configured && !quitting,
		},
		{
			ID:      MenuItemConnect,
			Action:  MenuActionConnect,
			Title:   connectTitle,
			Enabled: !quitting && (!options.Configured || options.Snapshot.State == StateReconnectRequired),
		},
		{
			ID:      MenuItemRestart,
			Action:  MenuActionRestart,
			Title:   "Restart daemon",
			Enabled: !quitting,
		},
		{
			ID:        MenuItemLaunchAtLogin,
			Action:    MenuActionToggleLaunchAtLogin,
			Title:     "Launch at login",
			Enabled:   !quitting,
			Checkable: true,
			Checked:   options.LaunchAtLogin,
		},
		{
			ID:      MenuItemOpenLogs,
			Action:  MenuActionOpenLogs,
			Title:   "Open logs",
			Enabled: !quitting,
		},
		{ID: MenuItemVersion, Title: "Version " + version},
		{
			ID:      MenuItemQuit,
			Action:  MenuActionQuit,
			Title:   "Quit Codesk",
			Enabled: !quitting,
		},
	}}
}

func statusTitle(state State, workspaceName string) string {
	status := "Unknown"
	switch state {
	case StateStarting:
		status = "Starting"
	case StateOnline:
		status = "Connected"
	case StateRetrying:
		status = "Reconnecting"
	case StateReconnectRequired:
		status = "Needs connection"
	case StateQuitting:
		status = "Quitting"
	}
	workspaceName = strings.TrimSpace(workspaceName)
	if workspaceName == "" {
		return "Codesk - " + status
	}
	return "Codesk - " + workspaceName + " - " + status
}

func (m MenuModel) Validate() error {
	if len(m.Items) != len(menuLayout) {
		return fmt.Errorf("desktop: menu has %d items, want %d", len(m.Items), len(menuLayout))
	}
	for index, expected := range menuLayout {
		item := m.Items[index]
		if item.ID != expected.id {
			return fmt.Errorf("desktop: menu item %d has ID %q, want %q", index, item.ID, expected.id)
		}
		if item.Action != expected.action {
			return fmt.Errorf("desktop: menu item %q has action %q, want %q", item.ID, item.Action, expected.action)
		}
		if strings.TrimSpace(item.Title) == "" {
			return fmt.Errorf("desktop: menu item %q has an empty title", item.ID)
		}
		if expected.action == MenuActionNone && item.Enabled {
			return fmt.Errorf("desktop: informational menu item %q is enabled", item.ID)
		}
		if item.Checked && !item.Checkable {
			return fmt.Errorf("desktop: non-checkable menu item %q is checked", item.ID)
		}
		if item.Checkable != (item.ID == MenuItemLaunchAtLogin) {
			return fmt.Errorf("desktop: menu item %q has invalid checkable state", item.ID)
		}
	}
	return nil
}

func (m MenuModel) Item(id MenuItemID) (MenuItem, error) {
	for _, item := range m.Items {
		if item.ID == id {
			return item, nil
		}
	}
	return MenuItem{}, fmt.Errorf("desktop: menu item %q not found", id)
}
