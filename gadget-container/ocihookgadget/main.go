package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	ocispec "github.com/opencontainers/runtime-spec/specs-go"

	"google.golang.org/grpc"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	pb "github.com/kinvolk/inspektor-gadget/pkg/gadgettracermanager/api"
	"github.com/kinvolk/inspektor-gadget/pkg/gadgettracermanager/containerutils"
)

var (
	socketfile string
	hook       string
	kubeconfig string
)

func init() {
	flag.StringVar(&socketfile, "socketfile", "/run/gadgettracermanager.socket", "Socket file")
	flag.StringVar(&hook, "hook", "", "OCI hook: prestart or poststop")
	flag.StringVar(&kubeconfig, "kubeconfig", "/etc/kubernetes/kubeconfig", "path to a kubeconfig")
}

func main() {
	// Parse arguments
	flag.Parse()
	if flag.NArg() > 0 {
		flag.PrintDefaults()
		panic(fmt.Errorf("invalid command"))
	}

	if hook != "prestart" && hook != "poststop" {
		panic(fmt.Errorf("hook %q not supported\n", hook))
	}

	// Parse state from stdin
	stateBuf, err := ioutil.ReadAll(os.Stdin)
	if err != nil {
		panic(fmt.Errorf("cannot read stdin: %v\n", err))
	}

	ociState := &ocispec.State{}
	err = json.Unmarshal(stateBuf, ociState)
	if err != nil {
		panic(fmt.Errorf("cannot parse stdin: %v\n%s\n", err, string(stateBuf)))
	}

	// Validate state
	if ociState.ID == "" || (ociState.Pid == 0 && hook == "prestart") {
		panic(fmt.Errorf("invalid OCI state: %+v", ociState))
	}

	// Connect to the Gadget Tracer Manager
	var client pb.GadgetTracerManagerClient
	var ctx context.Context
	var cancel context.CancelFunc
	conn, err := grpc.Dial("unix://"+socketfile, grpc.WithInsecure())
	if err != nil {
		panic(err)
	}
	defer conn.Close()
	client = pb.NewGadgetTracerManagerClient(conn)
	ctx, cancel = context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	// Handle the poststop hook first
	if hook == "poststop" {
		_, err := client.RemoveContainer(ctx, &pb.ContainerDefinition{
			ContainerId: ociState.ID,
		})
		if err != nil {
			panic(err)
		}
		return
	}

	// Get cgroup-v2 path
	cgroupPath, err := containerutils.GetCgroup2Path(ociState.Pid)
	if err != nil {
		panic(err)
	}

	// Get cgroup-v2 id
	cgroupId, err := containerutils.GetCgroupID(cgroupPath)
	if err != nil {
		panic(err)
	}

	// Get bundle directory and OCI spec (config.json)
	ppid := 0
	if statusFile, err := os.Open(filepath.Join("/proc", fmt.Sprintf("%d", ociState.Pid), "status")); err == nil {
		defer statusFile.Close()
		reader := bufio.NewReader(statusFile)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				break
			}
			if strings.HasPrefix(line, "PPid:\t") {
				ppidStr := strings.TrimPrefix(line, "PPid:\t")
				ppidStr = strings.TrimSuffix(ppidStr, "\n")
				ppid, err = strconv.Atoi(ppidStr)
				if err != nil {
					panic(fmt.Errorf("cannot parse ppid (%q): %v", ppidStr, err))
				}
				break
			}
		}
	} else {
		panic(fmt.Errorf("cannot parse /proc/PID/status: %v", err))
	}
	cmdline, err := ioutil.ReadFile(filepath.Join("/proc", fmt.Sprintf("%d", ppid), "cmdline"))
	if err != nil {
		panic(fmt.Errorf("cannot read /proc/PID/cmdline: %v", err))
	}
	cmdline = bytes.ReplaceAll(cmdline, []byte{0}, []byte("\n"))
	r := regexp.MustCompile("--bundle\n([^\n]*)\n")
	matches := r.FindStringSubmatch(string(cmdline))
	if len(matches) != 2 {
		panic(fmt.Errorf("cannot find bundle in %q: matches=%+v", string(cmdline), matches))
	}
	bundle := matches[1]
	bundleConfig, err := ioutil.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		panic(fmt.Errorf("cannot read config.json from bundle directory %q: %v", bundle, err))
	}

	ociSpec := &ocispec.Spec{}
	err = json.Unmarshal(bundleConfig, ociSpec)
	if err != nil {
		panic(fmt.Errorf("cannot parse config.json: %v\n%s\n", err, string(bundleConfig)))
	}

	// Get the pod from the API server
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		panic(err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	pods, err := clientset.CoreV1().Pods("").List(metav1.ListOptions{})
	if err != nil {
		panic(err.Error())
	}
	namespace := ""
	podname := ""
	containerIndex := -1
	labels := []*pb.Label{}
	for _, p := range pods.Items {
		uid := string(p.ObjectMeta.UID)
		uidWithUndescores := strings.ReplaceAll(uid, "-", "_")
		if !strings.Contains(cgroupPath, uidWithUndescores) {
			continue
		}
		namespace = p.ObjectMeta.Namespace
		podname = p.ObjectMeta.Name

		for k, v := range p.ObjectMeta.Labels {
			labels = append(labels, &pb.Label{Key: k, Value: v})
		}

		for i, container := range p.Spec.Containers {
			for _, m := range ociSpec.Mounts {
				pattern := fmt.Sprintf("pods/%s/containers/%s/", uid, container.Name)
				if strings.Contains(m.Source, pattern) {
					containerIndex = i
					break
				}
			}
		}
	}

	_, err = client.AddContainer(ctx, &pb.ContainerDefinition{
		ContainerId:    ociState.ID,
		CgroupPath:     cgroupPath,
		CgroupId:       cgroupId,
		Namespace:      namespace,
		Podname:        podname,
		ContainerIndex: int32(containerIndex),
		Labels:         labels,
	})
	if err != nil {
		panic(err)
	}
}
