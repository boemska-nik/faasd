package pkg

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"sort"

	"github.com/alexellis/k3sup/pkg/env"
	"github.com/compose-spec/compose-go/loader"
	compose "github.com/compose-spec/compose-go/types"
	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/oci"
	gocni "github.com/containerd/go-cni"
	"github.com/openfaas/faasd/pkg/cninetwork"
	"github.com/openfaas/faasd/pkg/service"
	"github.com/pkg/errors"

	"github.com/containerd/containerd/namespaces"
	"github.com/opencontainers/runtime-spec/specs-go"
)

const (
	defaultSnapshotter         = "overlayfs"
	workingDirectoryPermission = 0644
	// faasdNamespace is the containerd namespace services are created
	faasdNamespace         = "default"
	faasServicesPullAlways = false
)

type Service struct {
	Image  string
	Env    []string
	Name   string
	Mounts []Mount
	Caps   []string
	Args   []string
}

type Mount struct {
	Src  string
	Dest string
}

type Supervisor struct {
	client *containerd.Client
	cni    gocni.CNI
}

func NewSupervisor(sock string) (*Supervisor, error) {
	client, err := containerd.New(sock)
	if err != nil {
		return nil, err
	}

	cni, err := cninetwork.InitNetwork()
	if err != nil {
		return nil, err
	}

	return &Supervisor{
		client: client,
		cni:    cni,
	}, nil
}

func (s *Supervisor) Start(svcs []Service) error {
	ctx := namespaces.WithNamespace(context.Background(), faasdNamespace)

	wd, _ := os.Getwd()

	gw, err := cninetwork.CNIGateway()
	if err != nil {
		return err
	}
	hosts := fmt.Sprintf(`
127.0.0.1	localhost
%s	faasd-provider`, gw)

	writeHostsErr := ioutil.WriteFile(path.Join(wd, "hosts"),
		[]byte(hosts), workingDirectoryPermission)

	if writeHostsErr != nil {
		return fmt.Errorf("cannot write hosts file: %s", writeHostsErr)
	}

	images := map[string]containerd.Image{}

	for _, svc := range svcs {
		fmt.Printf("Preparing: %s with image: %s\n", svc.Name, svc.Image)

		img, err := service.PrepareImage(ctx, s.client, svc.Image, defaultSnapshotter, faasServicesPullAlways)
		if err != nil {
			return err
		}
		images[svc.Name] = img
		size, _ := img.Size(ctx)
		fmt.Printf("Prepare done for: %s, %d bytes\n", svc.Image, size)
	}

	for _, svc := range svcs {
		fmt.Printf("Reconciling: %s\n", svc.Name)

		containerErr := service.Remove(ctx, s.client, svc.Name)
		if containerErr != nil {
			return containerErr
		}

		image := images[svc.Name]

		mounts := []specs.Mount{}
		if len(svc.Mounts) > 0 {
			for _, mnt := range svc.Mounts {
				mounts = append(mounts, specs.Mount{
					Source:      mnt.Src,
					Destination: mnt.Dest,
					Type:        "bind",
					Options:     []string{"rbind", "rw"},
				})
			}

		}

		mounts = append(mounts, specs.Mount{
			Destination: "/etc/resolv.conf",
			Type:        "bind",
			Source:      path.Join(wd, "resolv.conf"),
			Options:     []string{"rbind", "ro"},
		})

		mounts = append(mounts, specs.Mount{
			Destination: "/etc/hosts",
			Type:        "bind",
			Source:      path.Join(wd, "hosts"),
			Options:     []string{"rbind", "ro"},
		})

		newContainer, containerCreateErr := s.client.NewContainer(
			ctx,
			svc.Name,
			containerd.WithImage(image),
			containerd.WithNewSnapshot(svc.Name+"-snapshot", image),
			containerd.WithNewSpec(oci.WithImageConfig(image),
				oci.WithCapabilities(svc.Caps),
				oci.WithMounts(mounts),
				withOCIArgs(svc.Args),
				oci.WithEnv(svc.Env)),
		)

		if containerCreateErr != nil {
			log.Printf("Error creating container %s\n", containerCreateErr)
			return containerCreateErr
		}

		log.Printf("Created container %s\n", newContainer.ID())

		task, err := newContainer.NewTask(ctx, cio.NewCreator(cio.WithStdio))
		if err != nil {
			log.Printf("Error creating task: %s\n", err)
			return err
		}

		labels := map[string]string{}
		network, err := cninetwork.CreateCNINetwork(ctx, s.cni, task, labels)

		if err != nil {
			return err
		}

		ip, err := cninetwork.GetIPAddress(network, task)
		if err != nil {
			return err
		}
		log.Printf("%s has IP: %s\n", newContainer.ID(), ip.String())

		hosts, _ := ioutil.ReadFile("hosts")

		hosts = []byte(string(hosts) + fmt.Sprintf(`
%s	%s
`, ip, svc.Name))
		writeErr := ioutil.WriteFile("hosts", hosts, workingDirectoryPermission)

		if writeErr != nil {
			log.Printf("Error writing file %s %s\n", "hosts", writeErr)
		}
		// os.Chown("hosts", 101, 101)

		_, err = task.Wait(ctx)
		if err != nil {
			log.Printf("Wait err: %s\n", err)
			return err
		}

		log.Printf("Task: %s\tContainer: %s\n", task.ID(), newContainer.ID())
		// log.Println("Exited: ", exitStatusC)

		if err = task.Start(ctx); err != nil {
			log.Printf("Task err: %s\n", err)
			return err
		}
	}

	return nil
}

func (s *Supervisor) Close() {
	defer s.client.Close()
}

func (s *Supervisor) Remove(svcs []Service) error {
	ctx := namespaces.WithNamespace(context.Background(), faasdNamespace)

	for _, svc := range svcs {
		err := cninetwork.DeleteCNINetwork(ctx, s.cni, s.client, svc.Name)
		if err != nil {
			log.Printf("[Delete] error removing CNI network for %s, %s\n", svc.Name, err)
			return err
		}

		err = service.Remove(ctx, s.client, svc.Name)
		if err != nil {
			return err
		}
	}
	return nil
}

func withOCIArgs(args []string) oci.SpecOpts {
	if len(args) > 0 {
		return oci.WithProcessArgs(args...)
	}

	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		return nil
	}
}

// ParseCompose converts a docker-compose Config into a service list that we can
// pass to the supervisor client Start.
//
// The only anticipated error is a failure if the value mounts are not of type 	`bind`.
func ParseCompose(config *compose.Config) ([]Service, error) {
	services := make([]Service, len(config.Services))
	for idx, s := range config.Services {
		// environment is a map[string]*string
		// but we want a []string

		var env []string

		envKeys := sortedEnvKeys(s.Environment)
		for _, name := range envKeys {
			value := s.Environment[name]
			if value == nil {
				env = append(env, fmt.Sprintf(`%s=""`, name))
			} else {
				env = append(env, fmt.Sprintf(`%s=%s`, name, *value))
			}
		}

		var mounts []Mount
		for _, v := range s.Volumes {
			if v.Type != "bind" {
				return nil, errors.Errorf("unsupported volume mount type '%s' when parsing service '%s'", v.Type, s.Name)
			}
			mounts = append(mounts, Mount{
				Src:  v.Source,
				Dest: v.Target,
			})
		}

		services[idx] = Service{
			Name:  s.Name,
			Image: s.Image,
			// ShellCommand is just an alias of string slice
			Args:   []string(s.Command),
			Caps:   s.CapAdd,
			Env:    env,
			Mounts: mounts,
		}
	}

	return services, nil
}

// LoadComposeFile is a helper method for loading a docker-compose file
func LoadComposeFile(wd string, file string) (*compose.Config, error) {
	file = path.Join(wd, file)
	b, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, err
	}

	config, err := loader.ParseYAML(b)
	if err != nil {
		return nil, err
	}

	archSuffix, err := GetArchSuffix(env.GetClientArch)
	if err != nil {
		return nil, err
	}

	var files []compose.ConfigFile
	files = append(files, compose.ConfigFile{Filename: file, Config: config})

	return loader.Load(compose.ConfigDetails{
		WorkingDir:  wd,
		ConfigFiles: files,
		Environment: map[string]string{
			"ARCH_SUFFIX": archSuffix,
		},
	})
}

func sortedEnvKeys(env map[string]*string) (keys []string) {
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ArchGetter provides client CPU architecture and
// client OS
type ArchGetter func() (string, string)

// GetArchSuffix provides client CPU architecture and
// client OS from ArchGetter
func GetArchSuffix(getClientArch ArchGetter) (suffix string, err error) {
	clientArch, clientOS := getClientArch()

	if clientOS != "Linux" {
		return "", fmt.Errorf("you can only use faasd with Linux")
	}

	switch clientArch {
	case "x86_64":
		// no suffix needed
		return "", nil
	case "armhf", "armv7l":
		return "-armhf", nil
	case "arm64", "aarch64":
		return "-arm64", nil
	default:
		// unknown, so use the default without suffix for now
		return "", nil
	}
}
