package metricspush

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/frappe/boat/internal/metrics"
	"github.com/frappe/boat/internal/model"
)

// RenderPrometheus renders the host and per-VM metrics as Prometheus exposition text
// (for a /metrics scrape). It reuses the same collection as the datum push, but adds
// the identity labels a scrape needs and no token carries: server on host samples,
// vm (the UUID) on per-VM samples. Lines are grouped by metric name with a single
// # TYPE, and everything is emitted in a deterministic order.
func RenderPrometheus(serverName string, host metrics.Metrics, vms []model.VirtualMachine, roots Roots) string {
	grouped := Collect("", serverName, host, vms, roots)

	type line struct {
		labels map[string]string
		value  float64
	}
	byMetric := map[string][]line{}
	var order []string
	add := func(name string, labels map[string]string, value float64) {
		if _, seen := byMetric[name]; !seen {
			order = append(order, name)
		}
		byMetric[name] = append(byMetric[name], line{labels: labels, value: value})
	}

	for _, sample := range grouped.Host {
		labels := map[string]string{"server": serverName}
		for key, value := range sample.Labels {
			labels[key] = value
		}
		add(sample.Metric, labels, sample.Value)
	}

	uuids := make([]string, 0, len(grouped.VMs))
	for uuid := range grouped.VMs {
		uuids = append(uuids, uuid)
	}
	sort.Strings(uuids)
	for _, uuid := range uuids {
		for _, sample := range grouped.VMs[uuid] {
			labels := map[string]string{"vm": uuid}
			for key, value := range sample.Labels {
				labels[key] = value
			}
			add(sample.Metric, labels, sample.Value)
		}
	}

	sort.Strings(order)
	var builder strings.Builder
	for _, name := range order {
		kind := "gauge"
		if strings.HasSuffix(name, "_total") {
			kind = "counter"
		}
		fmt.Fprintf(&builder, "# TYPE %s %s\n", name, kind)
		for _, entry := range byMetric[name] {
			fmt.Fprintf(&builder, "%s%s %s\n", name, renderLabels(entry.labels), strconv.FormatFloat(entry.value, 'g', -1, 64))
		}
	}
	return builder.String()
}

func renderLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		value := strings.ReplaceAll(labels[key], "\\", "\\\\")
		value = strings.ReplaceAll(value, "\"", "\\\"")
		value = strings.ReplaceAll(value, "\n", " ")
		pairs = append(pairs, key+`="`+value+`"`)
	}
	return "{" + strings.Join(pairs, ",") + "}"
}
