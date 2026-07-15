//go:build windows || darwin

package main

import (
	"context"
	"errors"
	"sync"

	"fyne.io/systray"

	"notty/daemon/internal/desktop"
)

type nativeTrayOptions struct {
	Initial      desktop.MenuModel
	Updates      <-chan desktop.MenuModel
	Actions      chan<- desktop.MenuAction
	Icon         []byte
	TemplateIcon []byte
	ReportError  func(error)
}

// runNativeTray blocks on the native status-item loop. It renders menu state
// and forwards typed actions only; the caller remains the lifecycle owner.
func runNativeTray(ctx context.Context, options nativeTrayOptions) error {
	if err := options.Initial.Validate(); err != nil {
		return err
	}
	if options.Actions == nil {
		return errors.New("codesk desktop: tray action channel is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var workers sync.WaitGroup
	// systray dispatches onReady asynchronously. The completion gate prevents
	// worker joins from racing its WaitGroup setup without blocking onReady from
	// onExit, which runs on the native event-loop thread.
	var lifecycleMu sync.Mutex
	readyStarted := false
	readyCompleted := false
	exiting := false
	readyDone := make(chan struct{})
	var finishReadyOnce sync.Once
	finishReady := func() {
		lifecycleMu.Lock()
		readyCompleted = true
		lifecycleMu.Unlock()
		finishReadyOnce.Do(func() { close(readyDone) })
	}
	var quitWorker sync.Once
	quitDone := make(chan struct{})
	systray.Run(func() {
		lifecycleMu.Lock()
		if exiting {
			lifecycleMu.Unlock()
			finishReady()
			return
		}
		readyStarted = true
		lifecycleMu.Unlock()
		defer finishReady()

		setNativeTrayIcon(options)
		items := buildNativeMenu(options.Initial)
		applyNativeMenu(items, options.Initial)

		for _, modelItem := range options.Initial.Items {
			if modelItem.Action == desktop.MenuActionNone {
				continue
			}
			nativeItem := items[modelItem.ID]
			workers.Add(1)
			go func(action desktop.MenuAction, clicked <-chan struct{}) {
				defer workers.Done()
				for {
					select {
					case _, ok := <-clicked:
						if !ok {
							return
						}
						select {
						case options.Actions <- action:
						case <-ctx.Done():
							return
						}
					case <-ctx.Done():
						return
					}
				}
			}(modelItem.Action, nativeItem.ClickedCh)
		}

		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case model, ok := <-options.Updates:
					if !ok {
						return
					}
					if err := model.Validate(); err != nil {
						reportNativeTrayError(options, err)
						continue
					}
					applyNativeMenu(items, model)
				case <-ctx.Done():
					return
				}
			}
		}()

		// The Quit caller is intentionally outside workers: Windows invokes
		// onExit synchronously from Quit, and waiting for this goroutine there
		// would deadlock it against itself.
		quitWorker.Do(func() {
			go func() {
				defer close(quitDone)
				<-ctx.Done()
				systray.Quit()
			}()
		})
	}, func() {
		lifecycleMu.Lock()
		exiting = true
		started := readyStarted
		completed := readyCompleted
		lifecycleMu.Unlock()
		if !started {
			finishReady()
			completed = true
		}
		quitWorker.Do(func() { close(quitDone) })
		cancel()
		// Darwin's tray setters synchronously dispatch to this event-loop
		// thread. Waiting for an in-flight onReady here would deadlock it. Once
		// onReady is complete, however, all WaitGroup additions are fixed and
		// the ordinary workers can be joined before native exit returns.
		if completed {
			workers.Wait()
		}
	})
	lifecycleMu.Lock()
	exiting = true
	started := readyStarted
	lifecycleMu.Unlock()
	if !started {
		finishReady()
	}
	cancel()
	quitWorker.Do(func() { close(quitDone) })
	<-readyDone
	workers.Wait()
	<-quitDone
	return nil
}

func setNativeTrayIcon(options nativeTrayOptions) {
	switch {
	case len(options.TemplateIcon) > 0:
		systray.SetTemplateIcon(options.TemplateIcon, options.Icon)
	case len(options.Icon) > 0:
		systray.SetIcon(options.Icon)
	}
	systray.SetTooltip("Codesk")
}

func buildNativeMenu(model desktop.MenuModel) map[desktop.MenuItemID]*systray.MenuItem {
	items := make(map[desktop.MenuItemID]*systray.MenuItem, len(model.Items))
	for index, item := range model.Items {
		if index == 1 || index == 4 || index == 6 || index == 7 {
			systray.AddSeparator()
		}
		if item.Checkable {
			items[item.ID] = systray.AddMenuItemCheckbox(item.Title, "", item.Checked)
		} else {
			items[item.ID] = systray.AddMenuItem(item.Title, "")
		}
	}
	return items
}

func applyNativeMenu(items map[desktop.MenuItemID]*systray.MenuItem, model desktop.MenuModel) {
	for _, modelItem := range model.Items {
		nativeItem := items[modelItem.ID]
		nativeItem.SetTitle(modelItem.Title)
		if modelItem.Checked {
			nativeItem.Check()
		} else {
			nativeItem.Uncheck()
		}
		if modelItem.Enabled {
			nativeItem.Enable()
		} else {
			nativeItem.Disable()
		}
	}
	if status := items[desktop.MenuItemStatus]; status != nil {
		systray.SetTooltip(statusTitle(model))
	}
}

func statusTitle(model desktop.MenuModel) string {
	status, err := model.Item(desktop.MenuItemStatus)
	if err != nil {
		return "Codesk"
	}
	return status.Title
}

func reportNativeTrayError(options nativeTrayOptions, err error) {
	if options.ReportError != nil {
		options.ReportError(err)
	}
}
