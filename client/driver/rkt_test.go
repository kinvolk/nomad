// +build linux

package driver

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/stretchr/testify/assert"

	ctestutils "github.com/hashicorp/nomad/client/testutil"
)

func TestRktVersionRegex(t *testing.T) {
	t.Parallel()
	if os.Getenv("NOMAD_TEST_RKT") == "" {
		t.Skip("NOMAD_TEST_RKT unset, skipping")
	}

	inputRkt := "rkt version 0.8.1"
	inputAppc := "appc version 1.2.0"
	expectedRkt := "0.8.1"
	expectedAppc := "1.2.0"
	rktMatches := reRktVersion.FindStringSubmatch(inputRkt)
	appcMatches := reAppcVersion.FindStringSubmatch(inputAppc)
	if rktMatches[1] != expectedRkt {
		fmt.Printf("Test failed; got %q; want %q\n", rktMatches[1], expectedRkt)
	}
	if appcMatches[1] != expectedAppc {
		fmt.Printf("Test failed; got %q; want %q\n", appcMatches[1], expectedAppc)
	}
}

// The fingerprinter test should always pass, even if rkt is not installed.
func TestRktDriver_Fingerprint(t *testing.T) {
	t.Parallel()
	if os.Getenv("NOMAD_TEST_RKT") == "" {
		t.Skip("skipping rkt tests")
	}

	ctestutils.RktCompatible(t)
	ctx := testDriverContexts(t, &structs.Task{Name: "foo", Driver: "rkt"})
	d := NewRktDriver(ctx.DriverCtx)
	node := &structs.Node{
		Attributes: make(map[string]string),
	}
	apply, err := d.Fingerprint(&config.Config{}, node)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !apply {
		t.Fatalf("should apply")
	}
	if node.Attributes["driver.rkt"] != "1" {
		t.Fatalf("Missing Rkt driver")
	}
	if node.Attributes["driver.rkt.version"] == "" {
		t.Fatalf("Missing Rkt driver version")
	}
	if node.Attributes["driver.rkt.appc.version"] == "" {
		t.Fatalf("Missing appc version for the Rkt driver")
	}
}

func TestRktDriver_Start_DNS_With_Image(t *testing.T) {
	if !testutil.IsTravis() {
		t.Parallel()
	}
	if os.Getenv("NOMAD_TEST_RKT") == "" {
		t.Skip("skipping rkt tests")
	}

	ctestutils.RktCompatible(t)
	// TODO: use test server to load from a fixture
	task := &structs.Task{
		Name:   "etcd",
		Driver: "rkt",
		Config: map[string]interface{}{
			"trust_prefix":       "coreos.com/etcd",
			"image":              "coreos.com/etcd:v2.0.4",
			"command":            "/etcd",
			"dns_servers":        []string{"8.8.8.8", "8.8.4.4"},
			"dns_search_domains": []string{"example.com", "example.org", "example.net"},
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: &structs.Resources{
			MemoryMB: 128,
			CPU:      100,
		},
	}

	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("error in prestart: %v", err)
	}
	resp, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Handle.Kill()

	// Attempt to open
	handle2, err := d.Open(ctx.ExecCtx, resp.Handle.ID())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if handle2 == nil {
		t.Fatalf("missing handle")
	}
	handle2.Kill()
}

func TestRktDriver_Start_DNS_With_SingleApp_PodManifest(t *testing.T) {
	if !testutil.IsTravis() {
		t.Parallel()
	}
	if os.Getenv("NOMAD_TEST_RKT") == "" {
		t.Skip("skipping rkt tests")
	}

	ctestutils.RktCompatible(t)
	task := &structs.Task{
		Name:   "etcd",
		Driver: "rkt",
		Config: map[string]interface{}{
			"pod_manifest": `{
				"acVersion": "1.27.0",
				"acKind": "PodManifest",
				"apps": [
					{
						"name": "coreos",
						"image": {
							"name": "coreos.com/etcd",
							"labels": [
								{
									"name":  "version",
									"value": "v2.0.4"
								}
							]
						},
						"app": {
							"exec": ["/etcd"],
							"user": "0",
							"group": "0"
						}
					}
				]
			}`,
			"dns_servers":        []string{"8.8.8.8", "8.8.4.4"},
			"dns_search_domains": []string{"example.com", "example.org", "example.net"},
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: &structs.Resources{
			MemoryMB: 128,
			CPU:      100,
		},
	}
	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)
	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("error in prestart: %v", err)
	}
	resp, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Handle.Kill()

	// Attempt to open
	handle2, err := d.Open(ctx.ExecCtx, resp.Handle.ID())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if handle2 == nil {
		t.Fatalf("missing handle")
	}
	handle2.Kill()
}

func TestRktDriver_Start_DNS_With_MultipleApps_PodManifest(t *testing.T) {
	if !testutil.IsTravis() {
		t.Parallel()
	}
	if os.Getenv("NOMAD_TEST_RKT") == "" {
		t.Skip("skipping rkt tests")
	}

	ctestutils.RktCompatible(t)
	task := &structs.Task{
		Name:   "etcd_nginx",
		Driver: "rkt",
		Config: map[string]interface{}{
			"pod_manifest": `{
				"acVersion": "1.27.0",
				"acKind": "PodManifest",
				"apps": [
					{
						"name": "coreos",
						"image": {
							"name": "coreos.com/etcd",
							"labels": [
								{
									"name":  "version",
									"value": "v2.0.4"
								}
							]
						},
						"app": {
							"exec": ["/etcd"],
							"user": "0",
							"group": "0"
						}
					},
					{
						"name": "nginx",
						"image": {
							"name": "docker://registry-1.docker.io/library/nginx"
						}
					}
				]
			}`,
			"dns_servers":        []string{"8.8.8.8", "8.8.4.4"},
			"dns_search_domains": []string{"example.com", "example.org", "example.net"},
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: &structs.Resources{
			MemoryMB: 128,
			CPU:      100,
		},
	}
	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)
	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("error in prestart: %v", err)
	}
	resp, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Handle.Kill()

	// Attempt to open
	handle2, err := d.Open(ctx.ExecCtx, resp.Handle.ID())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if handle2 == nil {
		t.Fatalf("missing handle")
	}
	handle2.Kill()
}

func TestRktDriver_Start_Wait_With_Image(t *testing.T) {
	if !testutil.IsTravis() {
		t.Parallel()
	}
	if os.Getenv("NOMAD_TEST_RKT") == "" {
		t.Skip("skipping rkt tests")
	}

	ctestutils.RktCompatible(t)
	task := &structs.Task{
		Name:   "etcd",
		Driver: "rkt",
		Config: map[string]interface{}{
			"trust_prefix": "coreos.com/etcd",
			"image":        "coreos.com/etcd:v2.0.4",
			"command":      "/etcd",
			"args":         []string{"--version"},
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: &structs.Resources{
			MemoryMB: 128,
			CPU:      100,
		},
	}

	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("error in prestart: %v", err)
	}
	resp, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	handle := resp.Handle.(*rktHandle)
	defer handle.Kill()

	// Update should be a no-op
	if err := handle.Update(task); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Signal should be an error
	if err := resp.Handle.Signal(syscall.SIGTERM); err == nil {
		t.Fatalf("err: %v", err)
	}

	select {
	case res := <-resp.Handle.WaitCh():
		if !res.Successful() {
			t.Fatalf("err: %v", res)
		}
	case <-time.After(time.Duration(testutil.TestMultiplier()*15) * time.Second):
		t.Fatalf("timeout")
	}

	// Make sure pod was removed #3561
	var stderr bytes.Buffer
	cmd := exec.Command(rktCmd, "status", handle.uuid)
	cmd.Stdout = ioutil.Discard
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatalf("expected error running 'rkt status %s' on removed container", handle.uuid)
	}
	if out := stderr.String(); !strings.Contains(out, "no matches found") {
		t.Fatalf("expected 'no matches found' but received: %s", out)
	}
}

func TestRktDriver_Start_Wait_With_SingleApp_PodManifest(t *testing.T) {
	if !testutil.IsTravis() {
		t.Parallel()
	}
	if os.Getenv("NOMAD_TEST_RKT") == "" {
		t.Skip("skipping rkt tests")
	}
	ctestutils.RktCompatible(t)
	task := &structs.Task{
		Name:   "etcd",
		Driver: "rkt",
		Config: map[string]interface{}{
			"pod_manifest": `{
				"acVersion": "1.27.0",
				"acKind": "PodManifest",
				"apps": [
					{
						"name": "coreos",
						"image": {
							"name": "coreos.com/etcd",
							"labels": [
								{
									"name":  "version",
									"value": "v2.0.4"
								}
							]
						},
						"app": {
								"exec": ["/etcd","--version"],
								"user": "0",
								"group": "0"
						}
					}
				]
			}`,
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: &structs.Resources{
			MemoryMB: 128,
			CPU:      100,
		},
	}
	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("error in prestart: %v", err)
	}
	resp, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	handle := resp.Handle.(*rktHandle)
	defer handle.Kill()

	// Update should be a no-op
	if err := handle.Update(task); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Signal should be an error
	if err := resp.Handle.Signal(syscall.SIGTERM); err == nil {
		t.Fatalf("err: %v", err)
	}

	select {
	case res := <-resp.Handle.WaitCh():
		if !res.Successful() {
			t.Fatalf("err: %v", res)
		}
	case <-time.After(time.Duration(testutil.TestMultiplier()*15) * time.Second):
		t.Fatalf("timeout")
	}

	// Make sure pod was removed #3561
	var stderr bytes.Buffer
	cmd := exec.Command(rktCmd, "status", handle.uuid)
	cmd.Stdout = ioutil.Discard
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatalf("expected error running 'rkt status %s' on removed container", handle.uuid)
	}
	if out := stderr.String(); !strings.Contains(out, "no matches found") {
		t.Fatalf("expected 'no matches found' but received: %s", out)
	}
}

func TestRktDriver_Start_Wait_With_MultipleApps_PodManifest(t *testing.T) {
	if !testutil.IsTravis() {
		t.Parallel()
	}
	if os.Getenv("NOMAD_TEST_RKT") == "" {
		t.Skip("skipping rkt tests")
	}
	ctestutils.RktCompatible(t)
	task := &structs.Task{
		Name:   "etcd_alpine",
		Driver: "rkt",
		Config: map[string]interface{}{
			"pod_manifest": `{
				"acVersion": "1.27.0",
				"acKind": "PodManifest",
				"apps": [
					{
						"name": "coreos",
						"image": {
							"name": "coreos.com/etcd",
							"labels": [
								{
									"name":  "version",
									"value": "v2.0.4"
								}
							]
						},
						"app": {
								"exec": ["/etcd","--version"],
								"user": "0",
								"group": "0"
						}
					},
					{
						"name": "alpine",
						"image": {
							"name": "docker://alpine"
						},
						"app": {
								"exec": ["/bin/sh"],
								"user": "0",
								"group": "0"
						}
					}
				]
			}`,
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: &structs.Resources{
			MemoryMB: 128,
			CPU:      100,
		},
	}
	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("error in prestart: %v", err)
	}
	resp, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	handle := resp.Handle.(*rktHandle)
	defer handle.Kill()

	// Update should be a no-op
	if err := handle.Update(task); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Signal should be an error
	if err := resp.Handle.Signal(syscall.SIGTERM); err == nil {
		t.Fatalf("err: %v", err)
	}

	select {
	case res := <-resp.Handle.WaitCh():
		if !res.Successful() {
			t.Fatalf("err: %v", res)
		}
	case <-time.After(time.Duration(testutil.TestMultiplier()*15) * time.Second):
		t.Fatalf("timeout")
	}

	// Make sure pod was removed #3561
	var stderr bytes.Buffer
	cmd := exec.Command(rktCmd, "status", handle.uuid)
	cmd.Stdout = ioutil.Discard
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatalf("expected error running 'rkt status %s' on removed container", handle.uuid)
	}
	if out := stderr.String(); !strings.Contains(out, "no matches found") {
		t.Fatalf("expected 'no matches found' but received: %s", out)
	}
}

func TestRktDriver_Start_Wait_Skip_Trust(t *testing.T) {
	if !testutil.IsTravis() {
		t.Parallel()
	}
	if os.Getenv("NOMAD_TEST_RKT") == "" {
		t.Skip("skipping rkt tests")
	}

	ctestutils.RktCompatible(t)
	task := &structs.Task{
		Name:   "etcd",
		Driver: "rkt",
		Config: map[string]interface{}{
			"image":   "coreos.com/etcd:v2.0.4",
			"command": "/etcd",
			"args":    []string{"--version"},
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: &structs.Resources{
			MemoryMB: 128,
			CPU:      100,
		},
	}

	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("error in prestart: %v", err)
	}
	resp, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Handle.Kill()

	// Update should be a no-op
	err = resp.Handle.Update(task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	select {
	case res := <-resp.Handle.WaitCh():
		if !res.Successful() {
			t.Fatalf("err: %v", res)
		}
	case <-time.After(time.Duration(testutil.TestMultiplier()*15) * time.Second):
		t.Fatalf("timeout")
	}
}

func TestRktDriver_Start_Wait_AllocDir_With_Image(t *testing.T) {
	if !testutil.IsTravis() {
		t.Parallel()
	}
	if os.Getenv("NOMAD_TEST_RKT") == "" {
		t.Skip("skipping rkt tests")
	}

	ctestutils.RktCompatible(t)

	exp := []byte{'w', 'i', 'n'}
	file := "output.txt"
	tmpvol, err := ioutil.TempDir("", "nomadtest_rktdriver_volumes")
	if err != nil {
		t.Fatalf("error creating temporary dir: %v", err)
	}
	defer os.RemoveAll(tmpvol)
	hostpath := filepath.Join(tmpvol, file)

	task := &structs.Task{
		Name:   "rkttest_alpine",
		Driver: "rkt",
		Config: map[string]interface{}{
			"image":   "docker://alpine",
			"command": "/bin/sh",
			"args": []string{
				"-c",
				fmt.Sprintf(`echo -n %s > foo/%s`, string(exp), file),
			},
			"net":     []string{"none"},
			"volumes": []string{fmt.Sprintf("%s:/foo", tmpvol)},
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: &structs.Resources{
			MemoryMB: 128,
			CPU:      100,
		},
	}

	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("error in prestart: %v", err)
	}
	resp, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Handle.Kill()

	select {
	case res := <-resp.Handle.WaitCh():
		if !res.Successful() {
			t.Fatalf("err: %v", res)
		}
	case <-time.After(time.Duration(testutil.TestMultiplier()*15) * time.Second):
		t.Fatalf("timeout")
	}
	// Check that data was written to the shared alloc directory.
	act, err := ioutil.ReadFile(hostpath)
	if err != nil {
		t.Fatalf("Couldn't read expected output: %v", err)
	}

	if !reflect.DeepEqual(act, exp) {
		t.Fatalf("Command output is %v; expected %v", act, exp)
	}
}

func TestRktDriver_Start_Wait_AllocDir_With_Single_App_PodManifest(t *testing.T) {
	if !testutil.IsTravis() {
		t.Parallel()
	}
	if os.Getenv("NOMAD_TEST_RKT") == "" {
		t.Skip("skipping rkt tests")
	}

	ctestutils.RktCompatible(t)

	exp := []byte{'w', 'i', 'n'}
	file := "output.txt"
	tmpvol, err := ioutil.TempDir("", "nomadtest_rktdriver_volumes")
	if err != nil {
		t.Fatalf("error creating temporary dir: %v", err)
	}
	defer os.RemoveAll(tmpvol)
	hostpath := filepath.Join(tmpvol, file)

	task := &structs.Task{
		Name:   "rkttest_alpine",
		Driver: "rkt",
		Config: map[string]interface{}{
			"pod_manifest": fmt.Sprintf(`{
				"acVersion": "1.27.0",
				"acKind": "PodManifest",
				"apps": [
					{
						"name": "alpine",
						"image": {
							"name": "docker://alpine"
						},
						"app": {
							"exec": [
                                "/bin/sh",
								"-c",
								"echo -n %s > foo/%s"
                            ],
							"user": "0",
							"group": "0",
							"mountPoints": [
								{
									"name": "tempvol",
									"path": "/foo"
								}
							]
						}
					}
				],
				"volumes": [
					{
					"name": "tempvol",
					"kind": "host",
					"source": "%s"
					}
				]
			}`, string(exp), file, tmpvol),
			"net": []string{"none"},
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: &structs.Resources{
			MemoryMB: 128,
			CPU:      100,
		},
	}

	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("error in prestart: %v", err)
	}
	resp, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer resp.Handle.Kill()

	select {
	case res := <-resp.Handle.WaitCh():
		if !res.Successful() {
			t.Fatalf("err: %v", res)
		}
	case <-time.After(time.Duration(testutil.TestMultiplier()*15) * time.Second):
		t.Fatalf("timeout")
	}
	// Check that data was written to the shared alloc directory.
	act, err := ioutil.ReadFile(hostpath)
	if err != nil {
		t.Fatalf("Couldn't read expected output: %v", err)
	}

	if !reflect.DeepEqual(act, exp) {
		t.Fatalf("Command output is %v; expected %v", act, exp)
	}
}

func TestRktDriverUser(t *testing.T) {
	assert := assert.New(t)
	if !testutil.IsTravis() {
		t.Parallel()
	}
	if os.Getenv("NOMAD_TEST_RKT") == "" {
		t.Skip("skipping rkt tests")
	}

	ctestutils.RktCompatible(t)
	task := &structs.Task{
		Name:   "etcd",
		Driver: "rkt",
		User:   "alice",
		Config: map[string]interface{}{
			"trust_prefix": "coreos.com/etcd",
			"image":        "coreos.com/etcd:v2.0.4",
			"command":      "/etcd",
			"args":         []string{"--version"},
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: &structs.Resources{
			MemoryMB: 128,
			CPU:      100,
		},
	}

	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)

	_, err := d.Prestart(ctx.ExecCtx, task)
	assert.Nil(err)
	resp, err := d.Start(ctx.ExecCtx, task)
	assert.Nil(err)
	defer resp.Handle.Kill()

	select {
	case res := <-resp.Handle.WaitCh():
		assert.False(res.Successful())
	case <-time.After(time.Duration(testutil.TestMultiplier()*15) * time.Second):
		t.Fatalf("timeout")
	}

}

func TestRktTrustPrefix(t *testing.T) {
	if !testutil.IsTravis() {
		t.Parallel()
	}
	if os.Getenv("NOMAD_TEST_RKT") == "" {
		t.Skip("skipping rkt tests")
	}
	ctestutils.RktCompatible(t)
	task := &structs.Task{
		Name:   "etcd",
		Driver: "rkt",
		Config: map[string]interface{}{
			"trust_prefix": "example.com/invalid",
			"image":        "coreos.com/etcd:v2.0.4",
			"command":      "/etcd",
			"args":         []string{"--version"},
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: &structs.Resources{
			MemoryMB: 128,
			CPU:      100,
		},
	}
	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("error in prestart: %v", err)
	}
	resp, err := d.Start(ctx.ExecCtx, task)
	if err == nil {
		resp.Handle.Kill()
		t.Fatalf("Should've failed")
	}
	msg := "Error running rkt trust"
	if !strings.Contains(err.Error(), msg) {
		t.Fatalf("Expecting '%v' in '%v'", msg, err)
	}
}

func TestRktTaskValidate_With_Image(t *testing.T) {
	t.Parallel()
	ctestutils.RktCompatible(t)
	task := &structs.Task{
		Name:   "etcd",
		Driver: "rkt",
		Config: map[string]interface{}{
			"trust_prefix":       "coreos.com/etcd",
			"image":              "coreos.com/etcd:v2.0.4",
			"command":            "/etcd",
			"args":               []string{"--version"},
			"dns_servers":        []string{"8.8.8.8", "8.8.4.4"},
			"dns_search_domains": []string{"example.com", "example.org", "example.net"},
		},
		Resources: basicResources,
	}
	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)

	if err := d.Validate(task.Config); err != nil {
		t.Fatalf("Validation error in TaskConfig : '%v'", err)
	}
}

func TestRktTaskValidate_With_SingleApp_PodManifest(t *testing.T) {
	t.Parallel()
	ctestutils.RktCompatible(t)
	task := &structs.Task{
		Name:   "etcd",
		Driver: "rkt",
		Config: map[string]interface{}{
			"pod_manifest": `{
				"acVersion": "1.27.0",
				"acKind": "PodManifest",
				"apps": [
					{
						"name": "coreos",
						"image": {
							"name": "coreos.com/etcd",
							"labels": [
								{
									"name":  "version",
									"value": "v2.0.4"
								}
							]
						},
						"app": {
								"exec": ["/etcd","--version"],
								"user": "0",
								"group": "0"
						}
					}
				]
			}`,
			"dns_servers":        []string{"8.8.8.8", "8.8.4.4"},
			"dns_search_domains": []string{"example.com", "example.org", "example.net"},
		},
		Resources: basicResources,
	}
	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)

	if err := d.Validate(task.Config); err != nil {
		t.Fatalf("Validation error in TaskConfig : '%v'", err)
	}

}

func TestRktTaskValidate_With_MultipleApps_PodManifest(t *testing.T) {
	t.Parallel()
	ctestutils.RktCompatible(t)
	task := &structs.Task{
		Name:   "etcd_alpine",
		Driver: "rkt",
		Config: map[string]interface{}{
			"pod_manifest": `{
				"acVersion": "1.27.0",
				"acKind": "PodManifest",
				"apps": [
					{
						"name": "coreos",
						"image": {
							"name": "coreos.com/etcd",
							"labels": [
								{
									"name":  "version",
									"value": "v2.0.4"
								}
							]
						},
						"app": {
								"exec": ["/etcd","--version"],
								"user": "0",
								"group": "0"
						}
					},
					{
						"name": "alpine",
						"image": {
							"name": "docker://alpine"
						},
						"app": {
								"exec": ["/bin/sh"],
								"user": "0",
								"group": "0"
						}
					}
				]
			}`,
			"dns_servers":        []string{"8.8.8.8", "8.8.4.4"},
			"dns_search_domains": []string{"example.com", "example.org", "example.net"},
		},
		Resources: basicResources,
	}
	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)

	if err := d.Validate(task.Config); err != nil {
		t.Fatalf("Validation error in TaskConfig : '%v'", err)
	}

}

func TestRktTaskValidate_Without_PodManifest_And_Image(t *testing.T) {
	assert := assert.New(t)
	t.Parallel()
	ctestutils.RktCompatible(t)
	task := &structs.Task{
		Name:   "etcd",
		Driver: "rkt",
		Config: map[string]interface{}{
			"trust_prefix":       "coreos.com/etcd",
			"command":            "/etcd",
			"args":               []string{"--version"},
			"dns_servers":        []string{"8.8.8.8", "8.8.4.4"},
			"dns_search_domains": []string{"example.com", "example.org", "example.net"},
		},
		Resources: basicResources,
	}
	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)

	err := d.Validate(task.Config)
	assert.NotNil(err)
}

func TestRktTaskValidate_With_PodManifest_And_Image(t *testing.T) {
	assert := assert.New(t)
	t.Parallel()
	ctestutils.RktCompatible(t)
	task := &structs.Task{
		Name:   "etcd",
		Driver: "rkt",
		Config: map[string]interface{}{
			"trust_prefix": "coreos.com/etcd",
			"image":        "coreos.com/etcd:v2.0.4",
			"pod_manifest": `{
				"acVersion": "1.27.0",
				"acKind": "PodManifest",
				"apps": [
					{
						"name": "coreos",
						"image": {
							"name": "coreos.com/etcd",
							"labels": [
								{
									"name":  "version",
									"value": "v2.0.4"
								}
							]
						},
						"app": {
								"exec": ["/etcd","--version"],
								"user": "0",
								"group": "0"
						}
					}
				]
			}`,
			"dns_servers":        []string{"8.8.8.8", "8.8.4.4"},
			"dns_search_domains": []string{"example.com", "example.org", "example.net"},
		},
		Resources: basicResources,
	}
	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)

	err := d.Validate(task.Config)
	assert.NotNil(err)

}

// TODO: Port Mapping test should be ran with proper ACI image and test the port access.
func TestRktDriver_PortsMapping_With_Image(t *testing.T) {
	if !testutil.IsTravis() {
		t.Parallel()
	}
	if os.Getenv("NOMAD_TEST_RKT") == "" {
		t.Skip("skipping rkt tests")
	}

	ctestutils.RktCompatible(t)
	task := &structs.Task{
		Name:   "etcd",
		Driver: "rkt",
		Config: map[string]interface{}{
			"image": "docker://redis:latest",
			"port_map": []map[string]string{
				{
					"main": "6379-tcp",
				},
			},
			"debug": "true",
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: &structs.Resources{
			MemoryMB: 256,
			CPU:      512,
			Networks: []*structs.NetworkResource{
				{
					IP:            "127.0.0.1",
					ReservedPorts: []structs.Port{{Label: "main", Value: 8080}},
				},
			},
		},
	}

	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("error in prestart: %v", err)
	}
	resp, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Network == nil {
		t.Fatalf("Expected driver to set a DriverNetwork, but it did not!")
	}

	failCh := make(chan error, 1)
	go func() {
		time.Sleep(1 * time.Second)
		if err := resp.Handle.Kill(); err != nil {
			failCh <- err
		}
	}()

	select {
	case err := <-failCh:
		t.Fatalf("failed to kill handle: %v", err)
	case <-resp.Handle.WaitCh():
	case <-time.After(time.Duration(testutil.TestMultiplier()*15) * time.Second):
		t.Fatalf("timeout")
	}
}

func TestRktDriver_PortsMapping_With_SingleApp_PodManifest(t *testing.T) {
	if !testutil.IsTravis() {
		t.Parallel()
	}
	if os.Getenv("NOMAD_TEST_RKT") == "" {
		t.Skip("skipping rkt tests")
	}

	ctestutils.RktCompatible(t)
	task := &structs.Task{
		Name:   "redis",
		Driver: "rkt",
		Config: map[string]interface{}{
			"pod_manifest": `{
				"acVersion": "1.27.0",
				"acKind": "PodManifest",
				"apps": [
					{
						"name": "redis",
						"image": {
							"name": "docker://redis"
						},
						"app": {
                             "exec": [
                                "docker-entrypoint.sh",
                                "redis-server"
                            ],
							"user": "0",
							"group": "0",
                            "environment": [
                                {
                                    "name": "PATH",
                                    "value": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
                                }
                            ],
							"ports": [
								{
									"name": "6379-tcp",
									"protocol": "tcp",
									"port": 6379,
									"count": 1,
									"socketActivated": false
								}
							]
						}
					}
				]
			}`,
			"port_map": []map[string]string{
				{
					"main": "6379-tcp",
				},
			},
			"debug": "true",
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: &structs.Resources{
			MemoryMB: 256,
			CPU:      512,
			Networks: []*structs.NetworkResource{
				{
					IP:            "127.0.0.1",
					ReservedPorts: []structs.Port{{Label: "main", Value: 8080}},
				},
			},
		},
	}

	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("error in prestart: %v", err)
	}
	resp, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Network == nil {
		t.Fatalf("Expected driver to set a DriverNetwork, but it did not!")
	}

	failCh := make(chan error, 1)
	go func() {
		time.Sleep(1 * time.Second)
		if err := resp.Handle.Kill(); err != nil {
			failCh <- err
		}
	}()

	select {
	case err := <-failCh:
		t.Fatalf("failed to kill handle: %v", err)
	case <-resp.Handle.WaitCh():
	case <-time.After(time.Duration(testutil.TestMultiplier()*15) * time.Second):
		t.Fatalf("timeout")
	}
}

func TestRktDriver_PortsMapping_With_MultipleApps_PodManifest(t *testing.T) {
	if !testutil.IsTravis() {
		t.Parallel()
	}
	if os.Getenv("NOMAD_TEST_RKT") == "" {
		t.Skip("skipping rkt tests")
	}

	ctestutils.RktCompatible(t)
	task := &structs.Task{
		Name:   "redis_nginx",
		Driver: "rkt",
		Config: map[string]interface{}{
			"pod_manifest": `{
				"acVersion": "1.27.0",
				"acKind": "PodManifest",
				"apps": [
					{
						"name": "redis",
						"image": {
							"name": "docker://redis"
						},
						"app": {
                             "exec": [
                                "docker-entrypoint.sh",
                                "redis-server"
                            ],
							"user": "0",
							"group": "0",
                            "environment": [
                                {
                                    "name": "PATH",
                                    "value": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
                                }
                            ],
							"ports": [
								{
									"name": "6379-tcp",
									"protocol": "tcp",
									"port": 6379,
									"count": 1,
									"socketActivated": false
								}
							]
						}
					},
					{
						"name": "nginx",
						"image": {
							"name": "docker://nginx"
						},
						"app": {
							"exec": [
								"nginx",
								"-g",
								"daemon off;"
							],
							"user": "0",
							"group": "0",
							"environment": [
								{
									"name": "PATH",
									"value": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
								}
							],
							"ports": [
								{
									"name": "80-tcp",
									"protocol": "tcp",
									"port": 80,
									"count": 1,
									"socketActivated": false
								}
							]
						}
					}
				]
			}`,
			"port_map": []map[string]string{
				{
					"db":     "6379-tcp",
					"server": "80-tcp",
				},
			},
			"debug": "true",
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: &structs.Resources{
			MemoryMB: 256,
			CPU:      512,
			Networks: []*structs.NetworkResource{
				{
					IP: "127.0.0.1",
					ReservedPorts: []structs.Port{
						{Label: "db", Value: 8080},
						{Label: "server", Value: 8081},
					},
				},
			},
		},
	}

	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("error in prestart: %v", err)
	}
	resp, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Network == nil {
		t.Fatalf("Expected driver to set a DriverNetwork, but it did not!")
	}

	failCh := make(chan error, 1)
	go func() {
		time.Sleep(1 * time.Second)
		if err := resp.Handle.Kill(); err != nil {
			failCh <- err
		}
	}()

	select {
	case err := <-failCh:
		t.Fatalf("failed to kill handle: %v", err)
	case <-resp.Handle.WaitCh():
	case <-time.After(time.Duration(testutil.TestMultiplier()*15) * time.Second):
		t.Fatalf("timeout")
	}
}

// TestRktDriver_PortsMapping_Host_With_Image asserts that port_map isn't required when
// host networking is used.
func TestRktDriver_PortsMapping_Host_With_Image(t *testing.T) {
	if !testutil.IsTravis() {
		t.Parallel()
	}
	if os.Getenv("NOMAD_TEST_RKT") == "" {
		t.Skip("skipping rkt tests")
	}

	ctestutils.RktCompatible(t)
	task := &structs.Task{
		Name:   "etcd",
		Driver: "rkt",
		Config: map[string]interface{}{
			"image": "docker://redis:latest",
			"net":   []string{"host"},
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: &structs.Resources{
			MemoryMB: 256,
			CPU:      512,
			Networks: []*structs.NetworkResource{
				{
					IP:            "127.0.0.1",
					ReservedPorts: []structs.Port{{Label: "main", Value: 8080}},
				},
			},
		},
	}

	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("error in prestart: %v", err)
	}
	resp, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Network != nil {
		t.Fatalf("No network should be returned with --net=host but found: %#v", resp.Network)
	}

	failCh := make(chan error, 1)
	go func() {
		time.Sleep(1 * time.Second)
		if err := resp.Handle.Kill(); err != nil {
			failCh <- err
		}
	}()

	select {
	case err := <-failCh:
		t.Fatalf("failed to kill handle: %v", err)
	case <-resp.Handle.WaitCh():
	case <-time.After(time.Duration(testutil.TestMultiplier()*15) * time.Second):
		t.Fatalf("timeout")
	}
}

// TestRktDriver_PortsMapping_Host_With_SingleApp_PodManifest asserts that port_map isn't required
// when host networking is used.
func TestRktDriver_PortsMapping_Host_With_SingleApp_PodManifest(t *testing.T) {
	if !testutil.IsTravis() {
		t.Parallel()
	}
	if os.Getenv("NOMAD_TEST_RKT") == "" {
		t.Skip("skipping rkt tests")
	}

	ctestutils.RktCompatible(t)
	task := &structs.Task{
		Name:   "redis",
		Driver: "rkt",
		Config: map[string]interface{}{
			"pod_manifest": `{
				"acVersion": "1.27.0",
				"acKind": "PodManifest",
				"apps": [
					{
						"name": "redis",
						"image": {
							"name": "docker://redis"
						}
					}
				]
			}`,
			"net": []string{"host"},
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: &structs.Resources{
			MemoryMB: 256,
			CPU:      512,
			Networks: []*structs.NetworkResource{
				{
					IP:            "127.0.0.1",
					ReservedPorts: []structs.Port{{Label: "main", Value: 8080}},
				},
			},
		},
	}

	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("error in prestart: %v", err)
	}
	resp, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Network != nil {
		t.Fatalf("No network should be returned with --net=host but found: %#v", resp.Network)
	}

	failCh := make(chan error, 1)
	go func() {
		time.Sleep(1 * time.Second)
		if err := resp.Handle.Kill(); err != nil {
			failCh <- err
		}
	}()

	select {
	case err := <-failCh:
		t.Fatalf("failed to kill handle: %v", err)
	case <-resp.Handle.WaitCh():
	case <-time.After(time.Duration(testutil.TestMultiplier()*15) * time.Second):
		t.Fatalf("timeout")
	}
}

// TestRktDriver_PortsMapping_Host_With_MultipleApps_PodManifest asserts that port_map isn't required
// when host networking is used.
func TestRktDriver_PortsMapping_Host_With_MultipleApps_PodManifest(t *testing.T) {
	if !testutil.IsTravis() {
		t.Parallel()
	}
	if os.Getenv("NOMAD_TEST_RKT") == "" {
		t.Skip("skipping rkt tests")
	}

	ctestutils.RktCompatible(t)
	task := &structs.Task{
		Name:   "redis_alpine",
		Driver: "rkt",
		Config: map[string]interface{}{
			"pod_manifest": `{
				"acVersion": "1.27.0",
				"acKind": "PodManifest",
				"apps": [
					{
						"name": "redis",
						"image": {
							"name": "docker://redis"
						}
					},
					{
						"name": "alpine",
						"image": {
							"name": "docker://alpine"
						}
					}
				]
			}`,
			"net": []string{"host"},
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: &structs.Resources{
			MemoryMB: 256,
			CPU:      512,
			Networks: []*structs.NetworkResource{
				{
					IP:            "127.0.0.1",
					ReservedPorts: []structs.Port{{Label: "main", Value: 8080}},
				},
			},
		},
	}

	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("error in prestart: %v", err)
	}
	resp, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Network != nil {
		t.Fatalf("No network should be returned with --net=host but found: %#v", resp.Network)
	}

	failCh := make(chan error, 1)
	go func() {
		time.Sleep(1 * time.Second)
		if err := resp.Handle.Kill(); err != nil {
			failCh <- err
		}
	}()

	select {
	case err := <-failCh:
		t.Fatalf("failed to kill handle: %v", err)
	case <-resp.Handle.WaitCh():
	case <-time.After(time.Duration(testutil.TestMultiplier()*15) * time.Second):
		t.Fatalf("timeout")
	}
}

func TestRktDriver_HandlerExec_With_Image(t *testing.T) {
	if !testutil.IsTravis() {
		t.Parallel()
	}
	if os.Getenv("NOMAD_TEST_RKT") == "" {
		t.Skip("skipping rkt tests")
	}

	ctestutils.RktCompatible(t)
	task := &structs.Task{
		Name:   "etcd",
		Driver: "rkt",
		Config: map[string]interface{}{
			"trust_prefix": "coreos.com/etcd",
			"image":        "coreos.com/etcd:v2.0.4",
			"command":      "/etcd",
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: &structs.Resources{
			MemoryMB: 128,
			CPU:      100,
		},
	}

	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("error in prestart: %v", err)
	}
	resp, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Give the pod a second to start
	time.Sleep(time.Second)

	// Exec a command that should work
	out, code, err := resp.Handle.Exec(context.TODO(), "/etcd", []string{"--version"})
	if err != nil {
		t.Fatalf("error exec'ing etcd --version: %v", err)
	}
	if code != 0 {
		t.Fatalf("expected `etcd --version` to succeed but exit code was: %d\n%s", code, string(out))
	}
	if expected := []byte("etcd version "); !bytes.HasPrefix(out, expected) {
		t.Fatalf("expected output to start with %q but found:\n%q", expected, out)
	}

	// Exec a command that should fail
	out, code, err = resp.Handle.Exec(context.TODO(), "/etcd", []string{"--kaljdshf"})
	if err != nil {
		t.Fatalf("error exec'ing bad command: %v", err)
	}
	if code == 0 {
		t.Fatalf("expected `stat` to fail but exit code was: %d", code)
	}
	if expected := "flag provided but not defined"; !bytes.Contains(out, []byte(expected)) {
		t.Fatalf("expected output to contain %q but found: %q", expected, out)
	}

	if err := resp.Handle.Kill(); err != nil {
		t.Fatalf("error killing handle: %v", err)
	}
}

func TestRktDriver_HandlerExec_With_SingleApp_PodManifest(t *testing.T) {
	if !testutil.IsTravis() {
		t.Parallel()
	}
	if os.Getenv("NOMAD_TEST_RKT") == "" {
		t.Skip("skipping rkt tests")
	}

	ctestutils.RktCompatible(t)
	task := &structs.Task{
		Name:   "etcd",
		Driver: "rkt",
		Config: map[string]interface{}{
			"pod_manifest": `{
				"acVersion": "1.27.0",
				"acKind": "PodManifest",
				"apps": [
					{
						"name": "coreos",
						"image": {
							"name": "coreos.com/etcd",
							"labels": [
								{
									"name":  "version",
									"value": "v2.0.4"
								}
							]
						},
						"app": {
								"exec": ["/etcd"],
								"user": "0",
								"group": "0"
						}
					}
				]
			}`,
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: &structs.Resources{
			MemoryMB: 128,
			CPU:      100,
		},
	}

	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("error in prestart: %v", err)
	}
	resp, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Give the pod a second to start
	time.Sleep(time.Second)

	// Exec a command that should work
	out, code, err := resp.Handle.Exec(context.TODO(), "/etcd", []string{"--version"})
	if err != nil {
		t.Fatalf("error exec'ing etcd --version: %v", err)
	}
	if code != 0 {
		t.Fatalf("expected `etcd --version` to succeed but exit code was: %d\n%s", code, string(out))
	}
	if expected := []byte("etcd version "); !bytes.HasPrefix(out, expected) {
		t.Fatalf("expected output to start with %q but found:\n%q", expected, out)
	}

	// Exec a command that should fail
	out, code, err = resp.Handle.Exec(context.TODO(), "/etcd", []string{"--kaljdshf"})
	if err != nil {
		t.Fatalf("error exec'ing bad command: %v", err)
	}
	if code == 0 {
		t.Fatalf("expected `stat` to fail but exit code was: %d", code)
	}
	if expected := "flag provided but not defined"; !bytes.Contains(out, []byte(expected)) {
		t.Fatalf("expected output to contain %q but found: %q", expected, out)
	}

	if err := resp.Handle.Kill(); err != nil {
		t.Fatalf("error killing handle: %v", err)
	}
}

// This, tests with just the first app in the pod manifest as the code
// currently picks up the first app by default when there are multiple apps in a pod manifest
func TestRktDriver_HandlerExec_With_MultipleApps_PodManifest(t *testing.T) {
	if !testutil.IsTravis() {
		t.Parallel()
	}
	if os.Getenv("NOMAD_TEST_RKT") == "" {
		t.Skip("skipping rkt tests")
	}

	ctestutils.RktCompatible(t)
	task := &structs.Task{
		Name:   "etcd_alpine",
		Driver: "rkt",
		Config: map[string]interface{}{
			"pod_manifest": `{
				"acVersion": "1.27.0",
				"acKind": "PodManifest",
				"apps": [
					{
						"name": "coreos",
						"image": {
							"name": "coreos.com/etcd",
							"labels": [
								{
									"name":  "version",
									"value": "v2.0.4"
								}
							]
						},
						"app": {
								"exec": ["/etcd"],
								"user": "0",
								"group": "0"
						}
					},
					{
						"name": "alpine",
						"image": {
							"name": "docker://alpine"
						},
						"app": {
								"exec": ["/bin/sh"],
								"user": "0",
								"group": "0"
						}
					}
				]
			}`,
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: &structs.Resources{
			MemoryMB: 128,
			CPU:      100,
		},
	}

	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)

	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("error in prestart: %v", err)
	}
	resp, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Give the pod a second to start
	time.Sleep(time.Second)

	// Exec a command on coreos app that should work
	out, code, err := resp.Handle.Exec(context.TODO(), "/etcd", []string{"--version"})
	if err != nil {
		t.Fatalf("error exec'ing etcd --version: %v", err)
	}
	if code != 0 {
		t.Fatalf("expected `etcd --version` to succeed but exit code was: %d\n%s", code, string(out))
	}
	if expected := []byte("etcd version "); !bytes.HasPrefix(out, expected) {
		t.Fatalf("expected output to start with %q but found:\n%q", expected, out)
	}

	// Exec a command on coreos app that should fail
	out, code, err = resp.Handle.Exec(context.TODO(), "/etcd", []string{"--kaljdshf"})
	if err != nil {
		t.Fatalf("error exec'ing bad command: %v", err)
	}
	if code == 0 {
		t.Fatalf("expected `stat` to fail but exit code was: %d", code)
	}
	if expected := "flag provided but not defined"; !bytes.Contains(out, []byte(expected)) {
		t.Fatalf("expected output to contain %q but found: %q", expected, out)
	}

	if err := resp.Handle.Kill(); err != nil {
		t.Fatalf("error killing handle: %v", err)
	}
}

func TestRktDriver_Remove_Error(t *testing.T) {
	if !testutil.IsTravis() {
		t.Parallel()
	}
	if os.Getenv("NOMAD_TEST_RKT") == "" {
		t.Skip("skipping rkt tests")
	}

	ctestutils.RktCompatible(t)

	// Removing a non-existent pod should return an error
	if err := rktRemove("00000000-0000-0000-0000-000000000000"); err == nil {
		t.Fatalf("expected an error")
	}

	if err := rktRemove("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"); err == nil {
		t.Fatalf("expected an error")
	}
}

// This test ensures that pod manifest can be used without errors when the optional fields are absent.
func TestRktDriver_PodManifest_Optional_Fields_Start(t *testing.T) {
	if !testutil.IsTravis() {
		t.Parallel()
	}
	if os.Getenv("NOMAD_TEST_RKT") == "" {
		t.Skip("skipping rkt tests")
	}

	ctestutils.RktCompatible(t)
	task := &structs.Task{
		Name:   "etcd",
		Driver: "rkt",
		Config: map[string]interface{}{
			"trust_prefix": "coreos.com/etcd",
			"pod_manifest": `{
				"acVersion": "1.27.0",
				"acKind": "PodManifest",
				"apps": [
					{
						"name": "coreos",
						"image": {
							"name": "coreos.com/etcd",
							"labels": [
								{
									"name":  "version",
									"value": "v2.0.4"
								}
							]
						}
					}
				]
			}`,
		},
		LogConfig: &structs.LogConfig{
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		},
		Resources: &structs.Resources{
			MemoryMB: 128,
			CPU:      100,
		},
	}
	ctx := testDriverContexts(t, task)
	defer ctx.AllocDir.Destroy()
	d := NewRktDriver(ctx.DriverCtx)
	if _, err := d.Prestart(ctx.ExecCtx, task); err != nil {
		t.Fatalf("error in prestart: %v", err)
	}
	resp, err := d.Start(ctx.ExecCtx, task)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp.Handle.Kill()
}
