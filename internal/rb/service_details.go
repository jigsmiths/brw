package rb

import "strings"

// ServiceDetails is what brew keeps on a formula's service block for `brew services info`.
type ServiceDetails struct {
	Defined bool
	Command []string
	Env     [][2]string
	// Fields holds working_dir, root_dir, input_path, log_path, error_log_path and interval, as written, when set.
	Fields    map[string]string
	Cron      [][2]string
	KeepAlive bool
	Timed     bool
}

var detailFields = []string{"working_dir", "root_dir", "input_path", "log_path", "error_log_path", "interval"}

// ServiceDetails reads the service block the way Homebrew::Service does. Cron is in brew's hash order.
func (f *Formula) ServiceDetails(root string) (d ServiceDetails, err error) {
	if f.b.serviceBlk == nil {
		return ServiceDetails{}, nil
	}
	defer catch(f.b.name, &err)
	s := f.renderService(root)
	d = ServiceDetails{Defined: true, Command: s.command(), Timed: s.timed(), Fields: map[string]string{}}
	ka := s.keepAlive()
	always, _ := kwVal(ka, "always")
	d.KeepAlive = len(ka.keys) > 0 && always != rbool(false)
	for _, kv := range s.env() {
		d.Env = append(d.Env, [2]string{kv[0], s.raw(rstr(kv[1]))})
	}
	for _, k := range detailFields {
		if v := s.get(k); v != nil {
			d.Fields[k] = s.raw(v)
		}
	}
	byName := map[string]string{}
	for _, kv := range s.cron() {
		byName[kv[0]] = kv[1]
	}
	if len(byName) > 0 {
		for _, k := range []string{"Month", "Day", "Weekday", "Hour", "Minute"} {
			d.Cron = append(d.Cron, [2]string{k, byName[k]})
		}
	}
	return d, nil
}

func (s renderedService) raw(v value) string {
	return strings.ReplaceAll(str(v), PrefixPlaceholder, s.root)
}
