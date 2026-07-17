package desktopacceptance

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

type InteractiveOperator struct {
	Input  io.Reader
	Output io.Writer
}

func (o InteractiveOperator) Perform(ctx context.Context, action Action, instruction string) error {
	if o.Input == nil || o.Output == nil {
		return errors.New("interactive operator input and output are required")
	}
	if _, err := fmt.Fprintf(o.Output, "\nACTION %s\n%s\nPress Enter only after the action is complete (or type BLOCKED and Enter): ", action, instruction); err != nil {
		return err
	}
	type response struct {
		value string
		err   error
	}
	result := make(chan response, 1)
	go func() {
		value, err := bufio.NewReader(o.Input).ReadString('\n')
		result <- response{value: strings.TrimSpace(value), err: err}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case response := <-result:
		if errors.Is(response.err, io.EOF) {
			return blockedError{"interactive operator input closed before confirmation"}
		}
		if response.err != nil {
			return response.err
		}
		if strings.EqualFold(response.value, "BLOCKED") {
			return blockedError{"operator reported the action blocked"}
		}
		if response.value != "" {
			return errors.New("press Enter without entering evidence or secrets")
		}
		return nil
	}
}
