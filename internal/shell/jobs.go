package shell

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/iamtaehyunpark/livegit/internal/proto"
	"github.com/iamtaehyunpark/livegit/internal/transport"
)

// This file is the ghost-side client for detached jobs (agent side:
// internal/agent/jobs.go). The job RPCs are plain request/response over the
// existing control stream; only log tailing needs a dedicated stream.

// StartJob launches cmd on Source as a detached job and returns the agent's
// response (id + launch mode + any warning). cwd is a rel dir under the mount.
func StartJob(client *transport.Client, cmd, cwd string) (proto.JobStartResp, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	f, err := client.ControlCall(ctx, proto.TypeJobStartReq, proto.JobStartReq{Cmd: cmd, Cwd: cwd})
	if err != nil {
		return proto.JobStartResp{}, err
	}
	var resp proto.JobStartResp
	if err := proto.Unmarshal(f.Body, &resp); err != nil {
		return proto.JobStartResp{}, err
	}
	return resp, nil
}

// ListJobs returns all jobs known to Source.
func ListJobs(client *transport.Client) ([]proto.JobInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	f, err := client.ControlCall(ctx, proto.TypeJobListReq, proto.JobListReq{})
	if err != nil {
		return nil, err
	}
	var resp proto.JobListResp
	if err := proto.Unmarshal(f.Body, &resp); err != nil {
		return nil, err
	}
	return resp.Jobs, nil
}

// ActJob performs "kill" or "rm" on a job and returns the agent's message.
func ActJob(client *transport.Client, id, action string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	f, err := client.ControlCall(ctx, proto.TypeJobActReq, proto.JobActReq{ID: id, Action: action})
	if err != nil {
		return "", err
	}
	var resp proto.JobActResp
	if err := proto.Unmarshal(f.Body, &resp); err != nil {
		return "", err
	}
	return resp.Message, nil
}

// StreamLog streams a job's log to stdout. With follow it tails until the job
// finishes; interrupt (Ctrl-C) simply ends the local process, which closes the
// stream and stops the tail on Source (it does NOT kill the job).
func StreamLog(client *transport.Client, id string, follow bool) error {
	stream, err := client.OpenJobLogStream()
	if err != nil {
		return err
	}
	defer stream.Close()
	hdr, _ := json.Marshal(proto.JobLogReq{ID: id, Follow: follow})
	if _, err := stream.Write(append(hdr, '\n')); err != nil {
		return err
	}
	_, err = io.Copy(os.Stdout, stream)
	return err
}

// FormatJobs renders `lg jobs` output. Kept here so the CLI stays a thin caller.
func FormatJobs(jobs []proto.JobInfo) string {
	if len(jobs) == 0 {
		return "no jobs\n"
	}
	out := fmt.Sprintf("%-8s  %-12s  %-6s  %-8s  %s\n", "ID", "STATE", "MODE", "AGE", "COMMAND")
	now := time.Now().Unix()
	anyDead := false
	for _, j := range jobs {
		state := j.State
		if j.State == "done" {
			state = fmt.Sprintf("done(%d)", j.Code)
		}
		anyDead = anyDead || j.State == "dead"
		out += fmt.Sprintf("%-8s  %-12s  %-6s  %-8s  %s\n",
			j.ID, state, j.Mode, humanAge(now-j.Started), j.Cmd)
	}
	if anyDead {
		// Diagnose the common cause up front: a job that vanished without an
		// exit code was almost always reaped by the server tearing down the
		// user's systemd instance when the last ssh session ended (Linger=no).
		out += "\ndead = exited without recording an exit code, usually because Source tore down\n" +
			"your user session (lingering off). lg enables `loginctl` lingering when starting\n" +
			"new jobs; check `loginctl show-user $USER -p Linger` on Source if this recurs.\n"
	}
	return out
}

func humanAge(secs int64) string {
	if secs < 0 {
		secs = 0
	}
	switch {
	case secs < 60:
		return fmt.Sprintf("%ds", secs)
	case secs < 3600:
		return fmt.Sprintf("%dm", secs/60)
	case secs < 86400:
		return fmt.Sprintf("%dh", secs/3600)
	default:
		return fmt.Sprintf("%dd", secs/86400)
	}
}
