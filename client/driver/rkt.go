// +build linux

package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	appcschema "github.com/appc/spec/schema"
	"github.com/appc/spec/schema/lastditch"
	appctypes "github.com/appc/spec/schema/types"
	rktv1 "github.com/rkt/rkt/api/v1"

	"github.com/hashicorp/go-plugin"
	"github.com/hashicorp/go-version"
	"github.com/hashicorp/nomad/client/allocdir"
	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/client/driver/env"
	"github.com/hashicorp/nomad/client/driver/executor"
	dstructs "github.com/hashicorp/nomad/client/driver/structs"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/helper/fields"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/mitchellh/mapstructure"
)

var (
	reRktVersion  = regexp.MustCompile(`rkt [vV]ersion[:]? (\d[.\d]+)`)
	reAppcVersion = regexp.MustCompile(`appc [vV]ersion[:]? (\d[.\d]+)`)
)

const (
	// minRktVersion is the earliest supported version of rkt. rkt added support
	// for CPU and memory isolators in 0.14.0. We cannot support an earlier
	// version to maintain an uniform interface across all drivers
	minRktVersion = "1.27.0"

	// The key populated in the Node Attributes to indicate the presence of the
	// Rkt driver
	rktDriverAttr = "driver.rkt"

	// rktVolumesConfigOption is the key for enabling the use of custom
	// bind volumes.
	rktVolumesConfigOption  = "rkt.volumes.enabled"
	rktVolumesConfigDefault = true

	// rktCmd is the command rkt is installed as.
	rktCmd = "rkt"

	// rktNetworkDeadline is how long to wait for container network to start
	rktNetworkDeadline = 5 * time.Second
)

// RktDriver is a driver for running images via Rkt
// We attempt to chose sane defaults for now, with more configuration available
// planned in the future
type RktDriver struct {
	DriverContext

	// A tri-state boolean to know if the fingerprinting has happened and
	// whether it has been successful
	fingerprintSuccess *bool
}

type RktDriverConfig struct {
	ImageName        string              `mapstructure:"image"`
	Command          string              `mapstructure:"command"`
	Args             []string            `mapstructure:"args"`
	TrustPrefix      string              `mapstructure:"trust_prefix"`
	DNSServers       []string            `mapstructure:"dns_servers"`        // DNS Server for containers
	DNSSearchDomains []string            `mapstructure:"dns_search_domains"` // DNS Search domains for containers
	Net              []string            `mapstructure:"net"`                // Networks for the containers
	PortMapRaw       []map[string]string `mapstructure:"port_map"`           //
	PortMap          map[string]string   `mapstructure:"-"`                  // A map of host port and the port name defined in the image manifest file
	Volumes          []string            `mapstructure:"volumes"`            // Host-Volumes to mount in, syntax: /path/to/host/directory:/destination/path/in/container[:readOnly]
	InsecureOptions  []string            `mapstructure:"insecure_options"`   // list of args for --insecure-options

	NoOverlay   bool   `mapstructure:"no_overlay"`   // disable overlayfs for rkt run
	Debug       bool   `mapstructure:"debug"`        // Enable debug option for rkt command
	PodManifest string `mapstructure:"pod_manifest"` // Allow for pod_manifest to be passed in
}

// rktHandle is returned from Start/Open as a handle to the PID
type rktHandle struct {
	uuid            string
	env             *env.TaskEnv
	taskDir         *allocdir.TaskDir
	pluginClient    *plugin.Client
	executorPid     int
	executor        executor.Executor
	logger          *log.Logger
	killTimeout     time.Duration
	maxKillTimeout  time.Duration
	waitCh          chan *dstructs.WaitResult
	doneCh          chan struct{}
	podManifestApps appcAppList // Added as a way to get podManifest apps in the Exec function from the Start function
}

// rktPID is a struct to map the pid running the process to the vm image on
// disk
type rktPID struct {
	UUID           string
	PluginConfig   *PluginReattachConfig
	ExecutorPid    int
	KillTimeout    time.Duration
	MaxKillTimeout time.Duration
}

// The appc spec makes the image ID mandatory. However while passing
// the pod_manifest in the nomad job spec, the ID will not be
// available. As a result we use lastditch.RuntimeApp to skip
// validation on the ID field initially and fallback to the appc
// implementation of RuntimeApp once the ID has been fetched and
// inserted into the pod manifest.
type appcRuntimeApp struct {
	lastditch.RuntimeApp
	App            *appctypes.App        `json:"app,omitempty"`
	ReadOnlyRootFS bool                  `json:"readOnlyRootFS,omitempty"`
	Mounts         []appcschema.Mount    `json:"mounts,omitempty"`
	Annotations    appctypes.Annotations `json:"annotations,omitempty"`
}

type appcAppList []appcRuntimeApp

type appcPodManifest struct {
	ACVersion       string                    `json:"acVersion"`
	ACKind          string                    `json:"acKind"`
	Apps            appcAppList               `json:"apps"`
	Volumes         []appctypes.Volume        `json:"volumes"`
	Isolators       []appctypes.Isolator      `json:"isolators"`
	Annotations     appctypes.Annotations     `json:"annotations"`
	Ports           []appctypes.ExposedPort   `json:"ports"`
	UserAnnotations appctypes.UserAnnotations `json:"userAnnotations,omitempty"`
	UserLabels      appctypes.UserLabels      `json:"userLabels,omitempty"`
}

// Retrieve pod status for the pod with the given UUID.
func rktGetStatus(uuid string) (*rktv1.Pod, error) {
	statusArgs := []string{
		"status",
		"--format=json",
		uuid,
	}
	var outBuf bytes.Buffer
	cmd := exec.Command(rktCmd, statusArgs...)
	cmd.Stdout = &outBuf
	cmd.Stderr = ioutil.Discard
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	var status rktv1.Pod
	if err := json.Unmarshal(outBuf.Bytes(), &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// Retrieves a pod manifest
func rktGetManifest(uuid string) (*appcschema.PodManifest, error) {
	statusArgs := []string{
		"cat-manifest",
		uuid,
	}
	var outBuf bytes.Buffer
	cmd := exec.Command(rktCmd, statusArgs...)
	cmd.Stdout = &outBuf
	cmd.Stderr = ioutil.Discard
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	var manifest appcschema.PodManifest
	if err := json.Unmarshal(outBuf.Bytes(), &manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func rktGetDriverNetwork(uuid string, driverConfigPortMap map[string]string) (*cstructs.DriverNetwork, error) {
	deadline := time.Now().Add(rktNetworkDeadline)
	var lastErr error
	for time.Now().Before(deadline) {
		if status, err := rktGetStatus(uuid); err == nil {
			for _, net := range status.Networks {
				if !net.IP.IsGlobalUnicast() {
					continue
				}

				// Get the pod manifest so we can figure out which ports are exposed
				var portmap map[string]int
				manifest, err := rktGetManifest(uuid)
				if err == nil {
					portmap, err = rktManifestMakePortMap(manifest, driverConfigPortMap)
					if err != nil {
						lastErr = fmt.Errorf("could not create manifest-based portmap: %v", err)
						return nil, lastErr
					}
				} else {
					lastErr = fmt.Errorf("could not get pod manifest: %v", err)
					return nil, lastErr
				}

				return &cstructs.DriverNetwork{
					PortMap: portmap,
					IP:      status.Networks[0].IP.String(),
				}, nil
			}

			if len(status.Networks) == 0 {
				lastErr = fmt.Errorf("no networks found")
			} else {
				lastErr = fmt.Errorf("no good driver networks out of %d returned", len(status.Networks))
			}
		} else {
			lastErr = fmt.Errorf("getting status failed: %v", err)
		}

		time.Sleep(400 * time.Millisecond)
	}
	return nil, fmt.Errorf("timed out, last error: %v", lastErr)
}

// Given a rkt/appc pod manifest and driver portmap configuration, create
// a driver portmap.
func rktManifestMakePortMap(manifest *appcschema.PodManifest, configPortMap map[string]string) (map[string]int, error) {
	if len(manifest.Apps) == 0 {
		return nil, fmt.Errorf("manifest has no apps")
	}

	portMap := make(map[string]int)
	for _, app := range manifest.Apps {
		if app.App == nil {
			return nil, fmt.Errorf("specified app %s has no App object", app.Name)
		}

		for svc, name := range configPortMap {
			for _, port := range app.App.Ports {
				if port.Name.String() == name {
					portMap[svc] = int(port.Port)
				}
			}
		}
	}
	return portMap, nil
}

// rktRemove pod after it has exited.
func rktRemove(uuid string) error {
	errBuf := &bytes.Buffer{}
	cmd := exec.Command(rktCmd, "rm", uuid)
	cmd.Stdout = ioutil.Discard
	cmd.Stderr = errBuf
	if err := cmd.Run(); err != nil {
		if msg := errBuf.String(); len(msg) > 0 {
			return fmt.Errorf("error removing pod %q: %s", uuid, msg)
		}
		return err
	}

	return nil
}

// NewRktDriver is used to create a new rkt driver
func NewRktDriver(ctx *DriverContext) Driver {
	return &RktDriver{DriverContext: *ctx}
}

func (d *RktDriver) FSIsolation() cstructs.FSIsolation {
	return cstructs.FSIsolationImage
}

// Validate is used to validate the driver configuration
func (d *RktDriver) Validate(config map[string]interface{}) error {
	fd := &fields.FieldData{
		Raw: config,
		Schema: map[string]*fields.FieldSchema{
			// Image is only required if pod_manifest was not passed in
			"image": {
				Type: fields.TypeString,
			},
			// pod_manifest is only required if image was not passed in
			"pod_manifest": {
				Type: fields.TypeString,
			},
			"command": {
				Type: fields.TypeString,
			},
			"args": {
				Type: fields.TypeArray,
			},
			"trust_prefix": {
				Type: fields.TypeString,
			},
			"dns_servers": {
				Type: fields.TypeArray,
			},
			"dns_search_domains": {
				Type: fields.TypeArray,
			},
			"net": {
				Type: fields.TypeArray,
			},
			"port_map": {
				Type: fields.TypeArray,
			},
			"debug": {
				Type: fields.TypeBool,
			},
			"volumes": {
				Type: fields.TypeArray,
			},
			"no_overlay": {
				Type: fields.TypeBool,
			},
			"insecure_options": {
				Type: fields.TypeArray,
			},
		},
	}

	if err := fd.Validate(); err != nil {
		return err
	}

	return nil
}

func (d *RktDriver) Abilities() DriverAbilities {
	return DriverAbilities{
		SendSignals: false,
		Exec:        true,
	}
}

func (d *RktDriver) Fingerprint(cfg *config.Config, node *structs.Node) (bool, error) {
	// Only enable if we are root when running on non-windows systems.
	if runtime.GOOS != "windows" && syscall.Geteuid() != 0 {
		if d.fingerprintSuccess == nil || *d.fingerprintSuccess {
			d.logger.Printf("[DEBUG] driver.rkt: must run as root user, disabling")
		}
		delete(node.Attributes, rktDriverAttr)
		d.fingerprintSuccess = helper.BoolToPtr(false)
		return false, nil
	}

	outBytes, err := exec.Command(rktCmd, "version").Output()
	if err != nil {
		delete(node.Attributes, rktDriverAttr)
		d.fingerprintSuccess = helper.BoolToPtr(false)
		return false, nil
	}
	out := strings.TrimSpace(string(outBytes))

	rktMatches := reRktVersion.FindStringSubmatch(out)
	appcMatches := reAppcVersion.FindStringSubmatch(out)
	if len(rktMatches) != 2 || len(appcMatches) != 2 {
		delete(node.Attributes, rktDriverAttr)
		d.fingerprintSuccess = helper.BoolToPtr(false)
		return false, fmt.Errorf("Unable to parse Rkt version string: %#v", rktMatches)
	}

	minVersion, _ := version.NewVersion(minRktVersion)
	currentVersion, _ := version.NewVersion(rktMatches[1])
	if currentVersion.LessThan(minVersion) {
		// Do not allow ancient rkt versions
		if d.fingerprintSuccess == nil {
			// Only log on first failure
			d.logger.Printf("[WARN] driver.rkt: unsupported rkt version %s; please upgrade to >= %s",
				currentVersion, minVersion)
		}
		delete(node.Attributes, rktDriverAttr)
		d.fingerprintSuccess = helper.BoolToPtr(false)
		return false, nil
	}

	node.Attributes[rktDriverAttr] = "1"
	node.Attributes["driver.rkt.version"] = rktMatches[1]
	node.Attributes["driver.rkt.appc.version"] = appcMatches[1]

	// Advertise if this node supports rkt volumes
	if d.config.ReadBoolDefault(rktVolumesConfigOption, rktVolumesConfigDefault) {
		node.Attributes["driver."+rktVolumesConfigOption] = "1"
	}
	d.fingerprintSuccess = helper.BoolToPtr(true)
	return true, nil
}

func (d *RktDriver) Periodic() (bool, time.Duration) {
	return true, 15 * time.Second
}

func (d *RktDriver) Prestart(ctx *ExecContext, task *structs.Task) (*PrestartResponse, error) {
	return nil, nil
}

// Run an existing Rkt image.
func (d *RktDriver) Start(ctx *ExecContext, task *structs.Task) (*StartResponse, error) {
	var driverConfig RktDriverConfig
	if err := mapstructure.WeakDecode(task.Config, &driverConfig); err != nil {
		return nil, err
	}

	driverConfig.PortMap = mapMergeStrStr(driverConfig.PortMapRaw...)

	// ACI image
	img := driverConfig.ImageName

	// Check if the pod manifest passed in is valid
	podManifest := appcPodManifest{}
	if img == "" {
		if err := json.Unmarshal([]byte(driverConfig.PodManifest), &podManifest); err != nil {
			return nil, fmt.Errorf("[ERR] driver.rkt: failed to unmarshal pod_manifest to appc schema: %s", err)
		}
	}

	// Global arguments given to both prepare and run-prepared
	globalArgs := make([]string, 0, 50)

	// Add debug option to rkt command.
	debug := driverConfig.Debug

	// Add the given trust prefix
	trustPrefix := driverConfig.TrustPrefix
	insecure := false
	if trustPrefix != "" {
		var outBuf, errBuf bytes.Buffer
		cmd := exec.Command(rktCmd, "trust", "--skip-fingerprint-review=true", fmt.Sprintf("--prefix=%s", trustPrefix), fmt.Sprintf("--debug=%t", debug))
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("Error running rkt trust: %s\n\nOutput: %s\n\nError: %s",
				err, outBuf.String(), errBuf.String())
		}
		d.logger.Printf("[DEBUG] driver.rkt: added trust prefix: %q", trustPrefix)
	} else {
		// Disble signature verification if the trust command was not run.
		insecure = true
	}

	// if we have a selective insecure_options, prefer them
	// insecure options are rkt's global argument, so we do this before the actual "run"
	if len(driverConfig.InsecureOptions) > 0 {
		globalArgs = append(globalArgs, fmt.Sprintf("--insecure-options=%s", strings.Join(driverConfig.InsecureOptions, ",")))
	} else if insecure {
		globalArgs = append(globalArgs, "--insecure-options=all")
	}

	// debug is rkt's global argument, so add it before the actual "run"
	globalArgs = append(globalArgs, fmt.Sprintf("--debug=%t", debug))

	prepareArgs := make([]string, 0, 50)
	runArgs := make([]string, 0, 50)

	prepareArgs = append(prepareArgs, globalArgs...)
	prepareArgs = append(prepareArgs, "prepare")
	runArgs = append(runArgs, globalArgs...)
	runArgs = append(runArgs, "run-prepared")

	// disable overlayfs
	if driverConfig.NoOverlay {
		prepareArgs = append(prepareArgs, "--no-overlay=true")
	}

	// Convert underscores to dashes in task names for use in volume names #2358
	sanitizedName := strings.Replace(task.Name, "_", "-", -1)

	// Format volume names appropriately
	allocVolName := fmt.Sprintf("%s-%s-alloc", d.DriverContext.allocID, sanitizedName)
	localVolName := fmt.Sprintf("%s-%s-local", d.DriverContext.allocID, sanitizedName)
	secretsVolName := fmt.Sprintf("%s-%s-secrets", d.DriverContext.allocID, sanitizedName)

	if img != "" {
		// Mount /alloc
		prepareArgs = append(prepareArgs, fmt.Sprintf("--volume=%s,kind=host,source=%s", allocVolName, ctx.TaskDir.SharedAllocDir))
		prepareArgs = append(prepareArgs, fmt.Sprintf("--mount=volume=%s,target=%s", allocVolName, allocdir.SharedAllocContainerPath))

		// Mount /local
		prepareArgs = append(prepareArgs, fmt.Sprintf("--volume=%s,kind=host,source=%s", localVolName, ctx.TaskDir.LocalDir))
		prepareArgs = append(prepareArgs, fmt.Sprintf("--mount=volume=%s,target=%s", localVolName, allocdir.TaskLocalContainerPath))

		// Mount /secrets
		prepareArgs = append(prepareArgs, fmt.Sprintf("--volume=%s,kind=host,source=%s", secretsVolName, ctx.TaskDir.SecretsDir))
		prepareArgs = append(prepareArgs, fmt.Sprintf("--mount=volume=%s,target=%s", secretsVolName, allocdir.TaskSecretsContainerPath))

		// Mount arbitrary volumes if enabled
		if len(driverConfig.Volumes) > 0 {
			if enabled := d.config.ReadBoolDefault(rktVolumesConfigOption, rktVolumesConfigDefault); !enabled {
				return nil, fmt.Errorf("%s is false; cannot use rkt volumes: %+q", rktVolumesConfigOption, driverConfig.Volumes)
			}
			for i, rawvol := range driverConfig.Volumes {
				parts := strings.Split(rawvol, ":")
				readOnly := "false"
				// job spec:
				//   volumes = ["/host/path:/container/path[:readOnly]"]
				// the third parameter is optional, mount is read-write by default
				if len(parts) == 3 {
					if parts[2] == "readOnly" {
						d.logger.Printf("[DEBUG] Mounting %s:%s as readOnly", parts[0], parts[1])
						readOnly = "true"
					} else {
						d.logger.Printf("[WARN] Unknown volume parameter '%s' ignored for mount %s", parts[2], parts[0])
					}
				} else if len(parts) != 2 {
					return nil, fmt.Errorf("invalid rkt volume: %q", rawvol)
				}
				volName := fmt.Sprintf("%s-%s-%d", d.DriverContext.allocID, sanitizedName, i)
				prepareArgs = append(prepareArgs, fmt.Sprintf("--volume=%s,kind=host,source=%s,readOnly=%s", volName, parts[0], readOnly))
				prepareArgs = append(prepareArgs, fmt.Sprintf("--mount=volume=%s,target=%s", volName, parts[1]))
			}
		}

		// Inject environment variables
		for k, v := range ctx.TaskEnv.Map() {
			prepareArgs = append(prepareArgs, fmt.Sprintf("--set-env=%s=%s", k, v))
		}

		// Image is set here, because the commands that follow apply to it
		prepareArgs = append(prepareArgs, img)

		// Check if the user has overridden the exec command.
		if driverConfig.Command != "" {
			prepareArgs = append(prepareArgs, fmt.Sprintf("--exec=%v", driverConfig.Command))
		}

		// Add memory isolator
		prepareArgs = append(prepareArgs, fmt.Sprintf("--memory=%vM", int64(task.Resources.MemoryMB)))

		// Add CPU isolator
		prepareArgs = append(prepareArgs, fmt.Sprintf("--cpu=%vm", int64(task.Resources.CPU)))

	} else {
		allocACName := appctypes.MustACName(allocVolName)
		localACName := appctypes.MustACName(localVolName)
		secretsACName := appctypes.MustACName(secretsVolName)

		nomadVolumes := []appctypes.Volume{
			{
				Name:   *allocACName,
				Kind:   "host",
				Source: ctx.TaskDir.SharedAllocDir,
			},
			{
				Name:   *localACName,
				Kind:   "host",
				Source: ctx.TaskDir.SharedAllocDir,
			},
			{
				Name:   *secretsACName,
				Kind:   "host",
				Source: ctx.TaskDir.SharedAllocDir,
			},
		}

		podManifest.Volumes = append(podManifest.Volumes, nomadVolumes...)

		mounts := []appcschema.Mount{
			{
				Volume: *allocACName,
				Path:   allocdir.SharedAllocContainerPath,
			},
			{
				Volume: *localACName,
				Path:   allocdir.TaskLocalContainerPath,
			},
			{
				Volume: *secretsACName,
				Path:   allocdir.TaskSecretsContainerPath,
			},
		}

		var environment appctypes.Environment
		for name, value := range ctx.TaskEnv.Map() {
			environment = append(environment, appctypes.EnvironmentVariable{
				Name:  name,
				Value: value,
			})
		}

		for i, app := range podManifest.Apps {
			ID, err := d.fetchImage(app.Image, globalArgs)
			if err != nil {
				return nil, err
			}

			currApp := &podManifest.Apps[i]

			d.logger.Printf("[DEBUG] driver.rkt: Image ID obtained: %s", ID)

			currApp.Image.ID = ID
			currApp.Image.Name = ""
			currApp.Mounts = append(currApp.Mounts, mounts...)

			if currApp.App != nil {
				if err := prepareApp(currApp.App, environment, task); err != nil {
					return nil, err
				}
			}
		}
	}

	// Add DNS servers
	if len(driverConfig.DNSServers) == 1 && (driverConfig.DNSServers[0] == "host" || driverConfig.DNSServers[0] == "none") {
		// Special case single item lists with the special values "host" or "none"
		runArgs = append(runArgs, fmt.Sprintf("--dns=%s", driverConfig.DNSServers[0]))
	} else {
		for _, ip := range driverConfig.DNSServers {
			if err := net.ParseIP(ip); err == nil {
				msg := fmt.Errorf("invalid ip address for container dns server %q", ip)
				d.logger.Printf("[DEBUG] driver.rkt: %v", msg)
				return nil, msg
			} else {
				runArgs = append(runArgs, fmt.Sprintf("--dns=%s", ip))
			}
		}
	}

	// set DNS search domains
	for _, domain := range driverConfig.DNSSearchDomains {
		runArgs = append(runArgs, fmt.Sprintf("--dns-search=%s", domain))
	}

	// set network
	network := strings.Join(driverConfig.Net, ",")
	if network != "" {
		runArgs = append(runArgs, fmt.Sprintf("--net=%s", network))
	}

	// Setup port mapping and exposed ports
	if len(task.Resources.Networks) == 0 {
		d.logger.Println("[DEBUG] driver.rkt: No network interfaces are available")
		if len(driverConfig.PortMap) > 0 {
			return nil, fmt.Errorf("Trying to map ports but no network interface is available")
		}
	} else if network == "host" {
		// Port mapping is skipped when host networking is used.
		d.logger.Println("[DEBUG] driver.rkt: Ignoring port_map when using --net=host")
	} else {
		// TODO add support for more than one network
		network := task.Resources.Networks[0]
		for _, port := range network.ReservedPorts {
			var containerPort string

			mapped, ok := driverConfig.PortMap[port.Label]
			if !ok {
				// If the user doesn't have a mapped port using port_map, driver stops running container.
				return nil, fmt.Errorf("port_map is not set. When you defined port in the resources, you need to configure port_map.")
			}
			containerPort = mapped

			d.logger.Printf("[DEBUG] driver.rkt: exposed port %s", containerPort)

			// Add port option to rkt run arguments. rkt allows multiple port args
			if img != "" {
				hostPortStr := strconv.Itoa(port.Value)
				prepareArgs = append(prepareArgs, fmt.Sprintf("--port=%s:%s", containerPort, hostPortStr))
			} else {
				containerPortACName, err := appctypes.NewACName(containerPort)
				if err != nil {
					return nil, fmt.Errorf("[ERR] driver.rkt: failed to set ACName for %s: %s", containerPort, err)
				}

				podManifest.Ports = append(podManifest.Ports, appctypes.ExposedPort{
					Name:     *containerPortACName,
					HostPort: uint(port.Value),
				})
			}
		}

		for _, port := range network.DynamicPorts {
			// By default we will map the allocated port 1:1 to the container
			var containerPort string

			if mapped, ok := driverConfig.PortMap[port.Label]; ok {
				containerPort = mapped
			} else {
				// If the user doesn't have mapped a port using port_map, driver stops running container.
				return nil, fmt.Errorf("port_map is not set. When you defined port in the resources, you need to configure port_map.")
			}

			d.logger.Printf("[DEBUG] driver.rkt: exposed port %s", containerPort)

			// Add port option to rkt run arguments. rkt allows multiple port args
			if img != "" {
				hostPortStr := strconv.Itoa(port.Value)
				prepareArgs = append(prepareArgs, fmt.Sprintf("--port=%s:%s", containerPort, hostPortStr))
			} else {
				containerPortACName, err := appctypes.NewACName(containerPort)
				if err != nil {
					return nil, fmt.Errorf("[ERR] driver.rkt: failed to set ACName for %s: %s", containerPort, err)
				}

				podManifest.Ports = append(podManifest.Ports, appctypes.ExposedPort{
					Name:     *containerPortACName,
					HostPort: uint(port.Value),
				})
			}
		}
	}

	if img != "" {
		// If a user has been specified for the task, pass it through to the user
		if task.User != "" {
			prepareArgs = append(prepareArgs, fmt.Sprintf("--user=%s", task.User))
		}
	}

	if img == "" {
		podManifestStr, err := json.Marshal(podManifest)
		if err != nil {
			return nil, fmt.Errorf("[ERR] driver.rkt: failed to marshal rkt pod manifest to JSON: %s", err)
		}

		// Ensure that the generated pod manifest is valid according to the appc spec
		validatePodManifest := appcschema.PodManifest{}
		if err := json.Unmarshal(podManifestStr, &validatePodManifest); err != nil {
			return nil, fmt.Errorf("[ERR] driver.rkt: Invalid rkt pod manifest: %s", err)
		}

		// Create temporary file to dump the contents of podManifest
		tmpfile, err := ioutil.TempFile("", "nomad-rkt-pod")
		if err != nil {
			return nil, err
		}

		podManifestTmpfile := tmpfile.Name()
		defer os.Remove(podManifestTmpfile)

		if _, err := tmpfile.Write(podManifestStr); err != nil {
			tmpfile.Close()
			return nil, err
		}
		tmpfile.Close()

		d.logger.Printf("[DEBUG] driver.rkt: wrote json to temp file %s", podManifestTmpfile)

		// pod manifest is set here
		prepareArgs = append(prepareArgs, fmt.Sprintf("--pod-manifest=%s", podManifestTmpfile))
	} else {
		// If a user has been specified for the task, pass it through to the user
		if task.User != "" {
			prepareArgs = append(prepareArgs, fmt.Sprintf("--user=%s", task.User))
		}
	}
	// Add user passed arguments.
	if len(driverConfig.Args) != 0 {
		parsed := ctx.TaskEnv.ParseAndReplace(driverConfig.Args)

		// Need to start arguments with "--"
		if len(parsed) > 0 {
			prepareArgs = append(prepareArgs, "--")
		}

		for _, arg := range parsed {
			prepareArgs = append(prepareArgs, fmt.Sprintf("%v", arg))
		}
	}

	pluginLogFile := filepath.Join(ctx.TaskDir.Dir, fmt.Sprintf("%s-executor.out", task.Name))
	executorConfig := &dstructs.ExecutorConfig{
		LogFile:  pluginLogFile,
		LogLevel: d.config.LogLevel,
	}

	execIntf, pluginClient, err := createExecutor(d.config.LogOutput, d.config, executorConfig)
	if err != nil {
		return nil, err
	}

	absPath, err := GetAbsolutePath(rktCmd)
	if err != nil {
		return nil, err
	}

	var outBuf, errBuf bytes.Buffer
	cmd := exec.Command(rktCmd, prepareArgs...)
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if img != "" {
		d.logger.Printf("[DEBUG] driver.rkt: preparing pod %q for task %q with: %v", img, d.taskName, prepareArgs)
	} else {
		d.logger.Printf("[DEBUG] driver.rkt: preparing pod with pod manifest for task %q with: %v", d.taskName, prepareArgs)
	}
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("Error preparing rkt pod: %s\n\nOutput: %s\n\nError: %s",
			err, outBuf.String(), errBuf.String())
	}
	uuid := strings.TrimSpace(outBuf.String())
	if img != "" {
		d.logger.Printf("[DEBUG] driver.rkt: pod %q for task %q prepared, UUID is: %s", img, d.taskName, uuid)
	} else {
		d.logger.Printf("[DEBUG] driver.rkt: pod with pod manifest for task %q prepared, UUID is: %s", d.taskName, uuid)
	}
	runArgs = append(runArgs, uuid)

	// The task's environment is set via --set-env flags above, but the rkt
	// command itself needs an evironment with PATH set to find iptables.
	eb := env.NewEmptyBuilder()
	filter := strings.Split(d.config.ReadDefault("env.blacklist", config.DefaultEnvBlacklist), ",")
	rktEnv := eb.SetHostEnvvars(filter).Build()
	executorCtx := &executor.ExecutorContext{
		TaskEnv: rktEnv,
		Driver:  "rkt",
		Task:    task,
		TaskDir: ctx.TaskDir.Dir,
		LogDir:  ctx.TaskDir.LogDir,
	}
	if err := execIntf.SetContext(executorCtx); err != nil {
		pluginClient.Kill()
		return nil, fmt.Errorf("failed to set executor context: %v", err)
	}

	execCmd := &executor.ExecCommand{
		Cmd:  absPath,
		Args: runArgs,
	}
	ps, err := execIntf.LaunchCmd(execCmd)
	if err != nil {
		pluginClient.Kill()
		return nil, err
	}

	if img != "" {
		d.logger.Printf("[DEBUG] driver.rkt: started ACI %q (UUID: %s) for task %q with: %v", img, uuid, d.taskName, runArgs)
	} else {
		d.logger.Printf("[DEBUG] driver.rkt: started ACI with pod manifest (UUID: %s) for task %q with: %v", uuid, d.taskName, runArgs)
	}

	maxKill := d.DriverContext.config.MaxKillTimeout
	h := &rktHandle{
		uuid:            uuid,
		env:             rktEnv,
		taskDir:         ctx.TaskDir,
		pluginClient:    pluginClient,
		executor:        execIntf,
		executorPid:     ps.Pid,
		logger:          d.logger,
		killTimeout:     GetKillTimeout(task.KillTimeout, maxKill),
		maxKillTimeout:  maxKill,
		doneCh:          make(chan struct{}),
		waitCh:          make(chan *dstructs.WaitResult, 1),
		podManifestApps: podManifest.Apps,
	}
	go h.run()

	// Only return a driver network if *not* using host networking
	var driverNetwork *cstructs.DriverNetwork
	if network != "host" {
		if img != "" {
			d.logger.Printf("[DEBUG] driver.rkt: retrieving network information for pod %q (UUID %s) for task %q", img, uuid, d.taskName)
		} else {
			d.logger.Printf("[DEBUG] driver.rkt: retrieving network information with pod manifest (UUID %s) for task %q", uuid, d.taskName)
		}
		driverNetwork, err = rktGetDriverNetwork(uuid, driverConfig.PortMap)
		if err != nil && !pluginClient.Exited() {
			if img != "" {
				d.logger.Printf("[WARN] driver.rkt: network status retrieval for pod %q (UUID %s) for task %q failed. Last error: %v", img, uuid, d.taskName, err)
			} else {
				d.logger.Printf("[WARN] driver.rkt: network status retrieval with pod manifest (UUID %s) for task %q failed. Last error: %v", uuid, d.taskName, err)
			}
			// If a portmap was given, this turns into a fatal error
			if len(driverConfig.PortMap) != 0 {
				pluginClient.Kill()
				return nil, fmt.Errorf("Trying to map ports but driver could not determine network information")
			}
		}
	}

	return &StartResponse{Handle: h, Network: driverNetwork}, nil
}

func (d *RktDriver) Cleanup(*ExecContext, *CreatedResources) error { return nil }

func (d *RktDriver) Open(ctx *ExecContext, handleID string) (DriverHandle, error) {
	// Parse the handle
	pidBytes := []byte(strings.TrimPrefix(handleID, "Rkt:"))
	id := &rktPID{}
	if err := json.Unmarshal(pidBytes, id); err != nil {
		return nil, fmt.Errorf("failed to parse Rkt handle '%s': %v", handleID, err)
	}

	pluginConfig := &plugin.ClientConfig{
		Reattach: id.PluginConfig.PluginConfig(),
	}
	exec, pluginClient, err := createExecutorWithConfig(pluginConfig, d.config.LogOutput)
	if err != nil {
		d.logger.Println("[ERR] driver.rkt: error connecting to plugin so destroying plugin pid and user pid")
		if e := destroyPlugin(id.PluginConfig.Pid, id.ExecutorPid); e != nil {
			d.logger.Printf("[ERR] driver.rkt: error destroying plugin and executor pid: %v", e)
		}
		return nil, fmt.Errorf("error connecting to plugin: %v", err)
	}

	// The task's environment is set via --set-env flags in Start, but the rkt
	// command itself needs an evironment with PATH set to find iptables.
	eb := env.NewEmptyBuilder()
	filter := strings.Split(d.config.ReadDefault("env.blacklist", config.DefaultEnvBlacklist), ",")
	rktEnv := eb.SetHostEnvvars(filter).Build()

	ver, _ := exec.Version()
	d.logger.Printf("[DEBUG] driver.rkt: version of executor: %v", ver.Version)
	// Return a driver handle
	h := &rktHandle{
		uuid:           id.UUID,
		env:            rktEnv,
		taskDir:        ctx.TaskDir,
		pluginClient:   pluginClient,
		executorPid:    id.ExecutorPid,
		executor:       exec,
		logger:         d.logger,
		killTimeout:    id.KillTimeout,
		maxKillTimeout: id.MaxKillTimeout,
		doneCh:         make(chan struct{}),
		waitCh:         make(chan *dstructs.WaitResult, 1),
	}
	go h.run()
	return h, nil
}

func (d *RktDriver) fetchImage(img lastditch.RuntimeImage, globalArgs []string) (string, error) {
	var version string
	for _, label := range img.Labels {
		if label.Name == "version" {
			version = label.Value
			break
		}
	}

	// If no version is supplied, set it to latest
	if version == "" {
		version = "latest"
	}

	imgName := fmt.Sprintf("%s:%s", img.Name, version)

	fetchArgs := make([]string, 0, len(globalArgs)+2)
	fetchArgs = append(fetchArgs, globalArgs...)
	fetchArgs = append(fetchArgs, []string{"fetch", imgName}...)

	var outBuf, errBuf bytes.Buffer
	cmd := exec.Command("rkt", fetchArgs...)
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	d.logger.Printf("[DEBUG] driver.rkt: fetching image %q for task %q with: %v", img, d.taskName, fetchArgs)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to fetch image: %s\n\nOutput: %s\n\nError: %s", err, outBuf.String(), errBuf.String())
	}

	return strings.TrimSpace(outBuf.String()), nil
}

func prepareApp(app *appctypes.App, environment appctypes.Environment, task *structs.Task) error {
	if app.Environment != nil {
		app.Environment = append(app.Environment, environment...)
	} else {
		app.Environment = environment
	}

	isolators := app.Isolators

	// Add memory isolator
	memoryACIdentifier := appctypes.MustACIdentifier(appctypes.ResourceMemoryName)
	if isolators.GetByName(*memoryACIdentifier) == nil {
		s := fmt.Sprintf("%vM", task.Resources.MemoryMB)
		resMemory, err := appctypes.NewResourceMemoryIsolator(s, s)
		if err != nil {
			return fmt.Errorf("[ERR] driver.rkt: failed to create ResourceMemory: %s", err)
		}

		isolators = append(isolators, resMemory.AsIsolator())
	}

	// Add CPU isolator
	cpuACIdentifier := appctypes.MustACIdentifier(appctypes.ResourceCPUName)
	if isolators.GetByName(*cpuACIdentifier) == nil {
		s := fmt.Sprintf("%vm", task.Resources.CPU)
		resCPU, err := appctypes.NewResourceCPUIsolator(s, s)
		if err != nil {
			return fmt.Errorf("[ERR] driver.rkt: failed to create ResourceCPU: %s", err)
		}

		isolators = append(isolators, resCPU.AsIsolator())
	}

	app.Isolators = isolators

	return nil
}

func (h *rktHandle) ID() string {
	// Return a handle to the PID
	pid := &rktPID{
		UUID:           h.uuid,
		PluginConfig:   NewPluginReattachConfig(h.pluginClient.ReattachConfig()),
		KillTimeout:    h.killTimeout,
		MaxKillTimeout: h.maxKillTimeout,
		ExecutorPid:    h.executorPid,
	}
	data, err := json.Marshal(pid)
	if err != nil {
		h.logger.Printf("[ERR] driver.rkt: failed to marshal rkt PID to JSON: %s", err)
	}
	return fmt.Sprintf("Rkt:%s", string(data))
}

func (h *rktHandle) WaitCh() chan *dstructs.WaitResult {
	return h.waitCh
}

func (h *rktHandle) Update(task *structs.Task) error {
	// Store the updated kill timeout.
	h.killTimeout = GetKillTimeout(task.KillTimeout, h.maxKillTimeout)
	h.executor.UpdateTask(task)

	// Update is not possible
	return nil
}

func (h *rktHandle) Exec(ctx context.Context, cmd string, args []string) ([]byte, int, error) {
	if h.uuid == "" {
		return nil, 0, fmt.Errorf("unable to find rkt pod UUID")
	}
	// enter + UUID + cmd + args...
	// but if we have pod manifest with multiple apps it becomes
	// enter + --<app=appname> + UUID + cmd + args...
	enterArgs := make([]string, 4+len(args))
	enterArgs[0] = "enter"
	// If we have multiple pod manifest, use the first app
	// TO DO: Rather than using the first app in the pod manifest by default,
	// let the required app's name be passed in.
	if len(h.podManifestApps) > 1 {
		enterArgs[1] = fmt.Sprintf("--app=%s", h.podManifestApps[0].Name)
		enterArgs[2] = h.uuid
		enterArgs[3] = cmd
		copy(enterArgs[4:], args)
	} else {
		enterArgs[1] = h.uuid
		enterArgs[2] = cmd
		copy(enterArgs[3:], args)
	}
	return executor.ExecScript(ctx, h.taskDir.Dir, h.env, nil, rktCmd, enterArgs)
}

func (h *rktHandle) Signal(s os.Signal) error {
	return fmt.Errorf("Rkt does not support signals")
}

// Kill is used to terminate the task. We send an Interrupt
// and then provide a 5 second grace period before doing a Kill.
func (h *rktHandle) Kill() error {
	h.executor.ShutDown()
	select {
	case <-h.doneCh:
		return nil
	case <-time.After(h.killTimeout):
		return h.executor.Exit()
	}
}

func (h *rktHandle) Stats() (*cstructs.TaskResourceUsage, error) {
	return nil, DriverStatsNotImplemented
}

func (h *rktHandle) run() {
	ps, werr := h.executor.Wait()
	close(h.doneCh)
	if ps.ExitCode == 0 && werr != nil {
		if e := killProcess(h.executorPid); e != nil {
			h.logger.Printf("[ERR] driver.rkt: error killing user process: %v", e)
		}
	}

	// Exit the executor
	if err := h.executor.Exit(); err != nil {
		h.logger.Printf("[ERR] driver.rkt: error killing executor: %v", err)
	}
	h.pluginClient.Kill()

	// Remove the pod
	if err := rktRemove(h.uuid); err != nil {
		h.logger.Printf("[ERR] driver.rkt: error removing pod %q - must gc manually: %v", h.uuid, err)
	} else {
		h.logger.Printf("[DEBUG] driver.rkt: removed pod %q", h.uuid)
	}

	// Send the results
	h.waitCh <- dstructs.NewWaitResult(ps.ExitCode, 0, werr)
	close(h.waitCh)
}
