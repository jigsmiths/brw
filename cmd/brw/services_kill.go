package main

import (
	"fmt"
	"strings"
)

func (e *env) servicesKill(targets []serviceTarget) error {
	for _, t := range targets {
		s, err := e.snapshot(t)
		if err != nil {
			return err
		}
		switch {
		case !s.info.Running():
			_, _ = fmt.Fprintf(e.out, "Service `%s` is not started.\n", t.name)
			continue
		case t.block.KeepAlive:
			_, _ = fmt.Fprintf(e.out, "Service `%s` is set to automatically restart and can't be killed.\n", t.name)
			continue
		}
		_, _ = fmt.Fprintf(e.out, "Killing `%s`... (might take a while)\n", t.name)
		names := s.loadedNames
		if len(names) == 0 {
			names = []string{s.active}
		}
		for _, name := range names {
			e.echoQuiet("stop", name)
			_ = e.launchctl.Stop(name)
		}
		after, err := e.snapshot(t)
		if err != nil {
			return err
		}
		if after.info.Running() {
			e.opoo("Unable to kill `%s` (label: %s)", t.name, strings.Join(names, ", "))
		} else {
			e.ohai("Successfully killed `%s` (label: %s)", t.name, strings.Join(names, ", "))
		}
	}
	return nil
}
