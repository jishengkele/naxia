//go:build linux

package taskproc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type processIdentity struct {
	pid   int
	start uint64
}

// StopForDelete also follows live parent links for children that discarded
// their environment. A workspace path alone grants no authority.
func (r *Registry) StopForDelete(ctx context.Context, taskID int64, roots, workspaceRoots []string) (bool, error) {
	adopted := make(map[processIdentity]Process)
	return stopProcesses(ctx, taskID, func() ([]Process, error) {
		tagged, err := r.List(taskID)
		if err != nil {
			return nil, err
		}
		owned := make(map[int]Process, len(tagged)+len(adopted))
		for _, p := range tagged {
			owned[p.PID] = p
		}
		// Once proven, keep the exact identity while stopping the tree.
		// A launcher may exit and reparent a child during graceful shutdown.
		for id, p := range adopted {
			stat, err := readProcessStat(p.PID)
			if vanished(err) || err == nil && (stat.start != p.StartTicks || stat.state == "Z" || stat.state == "X") {
				delete(adopted, id)
				continue
			}
			if err != nil {
				return nil, err
			}
			token, err := marker(p.PID)
			if vanished(err) {
				delete(adopted, id)
				continue
			}
			if err != nil {
				return nil, err
			}
			if token != "" {
				return nil, fmt.Errorf("进程 %d 的任务归属发生变化", p.PID)
			}
			owned[p.PID] = p
		}
		if err := collectDeleteChildren(taskID, owned, adopted); err != nil {
			return nil, err
		}
		refs, err := References(roots)
		if err != nil {
			return nil, err
		}
		var blockers DeleteBlockers
		for _, ref := range refs {
			if p, ok := owned[ref.PID]; ok && p.StartTicks == ref.StartTicks {
				continue
			}
			id := processIdentity{ref.PID, ref.StartTicks}
			if belongsToOwnedTree(id, owned) {
				p := Process{PID: ref.PID, StartTicks: ref.StartTicks, Name: ref.Name, TaskID: taskID}
				owned[p.PID] = p
				adopted[id] = p
				continue
			}
			blockers = append(blockers, ref)
		}
		if len(blockers) > 0 {
			return nil, blockers
		}
		if len(owned) == 0 {
			return nil, nil
		}
		selected := make(map[int]uint64, len(owned))
		var processes []Process
		for _, p := range owned {
			selected[p.PID] = p.StartTicks
			processes = append(processes, p)
		}
		shared, err := references(workspaceRoots, roots, selected)
		if err != nil {
			return nil, err
		}
		if len(shared) > 0 {
			return nil, DeleteBlockers(shared)
		}
		// Stop unmarked children before their tagged launchers.
		sort.Slice(processes, func(i, j int) bool {
			iChild, jChild := processes[i].token == "", processes[j].token == ""
			if iChild != jChild {
				return iChild
			}
			return processes[i].PID < processes[j].PID
		})
		return processes, nil
	})
}

func collectDeleteChildren(taskID int64, owned map[int]Process, adopted map[processIdentity]Process) error {
	if len(owned) == 0 {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	children := make(map[int][]processIdentity)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 || pid == os.Getpid() {
			continue
		}
		stat, err := readProcessStat(pid)
		if vanished(err) {
			continue
		}
		if err != nil {
			return err
		}
		if stat.state != "Z" && stat.state != "X" {
			children[stat.parent] = append(children[stat.parent], processIdentity{pid, stat.start})
		}
	}
	queue := make([]int, 0, len(owned))
	for pid := range owned {
		queue = append(queue, pid)
	}
	for i := 0; i < len(queue); i++ {
		for _, child := range children[queue[i]] {
			if _, ok := owned[child.pid]; ok || !belongsToOwnedTree(child, owned) {
				continue
			}
			raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(child.pid), "comm"))
			if vanished(err) {
				continue
			}
			if err != nil {
				return err
			}
			p := Process{PID: child.pid, StartTicks: child.start, Name: strings.TrimSpace(string(raw)), TaskID: taskID}
			owned[child.pid] = p
			adopted[child] = p
			queue = append(queue, child.pid)
		}
	}
	return nil
}

func belongsToOwnedTree(child processIdentity, owned map[int]Process) bool {
	pid, start := child.pid, child.start
	visited := make(map[int]bool)
	for pid > 1 && pid != os.Getpid() && !visited[pid] {
		visited[pid] = true
		stat, err := readProcessStat(pid)
		if err != nil || stat.start != start || stat.state == "Z" || stat.state == "X" {
			return false
		}
		token, err := marker(pid)
		if err != nil {
			return false
		}
		if p, ok := owned[pid]; ok && p.StartTicks == start {
			return token == p.token
		}
		if token != "" || stat.parent <= 1 {
			return false
		}
		parent, err := readProcessStat(stat.parent)
		if err != nil || parent.start > stat.start {
			return false
		}
		// Recheck the link after reading the parent to reject PID reuse or
		// concurrent reparenting during discovery.
		again, err := readProcessStat(pid)
		if err != nil || again.start != stat.start || again.parent != stat.parent {
			return false
		}
		pid, start = stat.parent, parent.start
	}
	return false
}
