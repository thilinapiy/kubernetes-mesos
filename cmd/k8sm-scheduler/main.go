/*
Copyright 2014 Google Inc. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// apiserver is the main api server and master for the cluster.
// it is responsible for serving the cluster management API.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os/user"
	"strconv"
	"strings"
	"time"

	_ "github.com/mesosphere/kubernetes-mesos/pkg/profile"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client/record"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/clientauth"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/kubelet"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/labels"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/master/ports"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/tools"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/version/verflag"
	"github.com/gogo/protobuf/proto"
	log "github.com/golang/glog"
	"github.com/mesos/mesos-go/auth"
	"github.com/mesos/mesos-go/auth/sasl"
	"github.com/mesos/mesos-go/auth/sasl/mech"
	mesos "github.com/mesos/mesos-go/mesosproto"
	mutil "github.com/mesos/mesos-go/mesosutil"
	bindings "github.com/mesos/mesos-go/scheduler"
	kmcloud "github.com/mesosphere/kubernetes-mesos/pkg/cloud/mesos"
	"github.com/mesosphere/kubernetes-mesos/pkg/executor/config"
	"github.com/mesosphere/kubernetes-mesos/pkg/scheduler"
	sconfig "github.com/mesosphere/kubernetes-mesos/pkg/scheduler/config"
	"github.com/mesosphere/kubernetes-mesos/pkg/scheduler/meta"
	"golang.org/x/net/context"
)

const (
	defaultMesosUser = "root" // should have privs to execute docker and iptables commands
)

var (
	port            = flag.Int("port", ports.SchedulerPort, "The port that the scheduler's http service runs on")
	address         = util.IP(net.ParseIP("127.0.0.1"))
	etcdServerList  util.StringList
	etcdConfigFile  = flag.String("etcd_config", "", "The config file for the etcd client. Mutually exclusive with -etcd_servers.")
	allowPrivileged = flag.Bool("allow_privileged", false, "If true, allow privileged containers. Default false.")
	authPath        = flag.String("auth_path", "", "Path to .kubernetes_auth file, specifying how to authenticate to API server.")
	apiServerList   util.StringList

	executorPath         = flag.String("executor_path", "", "Location of the kubernetes executor executable")
	proxyPath            = flag.String("proxy_path", "", "Location of the kubernetes proxy executable")
	mesosUser            = flag.String("mesos_user", "", "Mesos user for this framework, defaults to the username that owns the framework process.")
	mesosRole            = flag.String("mesos_role", "", "Mesos role for this framework, defaults to none.")
	mesosAuthPrincipal   = flag.String("mesos_authentication_principal", "", "Mesos authentication principal.")
	mesosAuthSecretFile  = flag.String("mesos_authentication_secret_file", "", "Mesos authentication secret file.")
	checkpoint           = flag.Bool("checkpoint", false, "Enable/disable checkpointing for the kubernetes-mesos framework.")
	failoverTimeout      = flag.Float64("failover_timeout", time.Duration((1<<62)-1).Seconds(), fmt.Sprintf("Framework failover timeout, in ns."))
	executorBindall      = flag.Bool("executor_bindall", false, "When true will set -address and -hostname_override of the executor to 0.0.0.0. Defaults to false.")
	executorRunProxy     = flag.Bool("executor_run_proxy", true, "Run the kube-proxy as a child process of the executor. Defaults to true.")
	executorProxyBindall = flag.Bool("executor_proxy_bindall", false, "When true pass -proxy_bindall to the executor. Defaults to false.")
	authProvider         = flag.String("mesos_authentication_provider", sasl.ProviderName, fmt.Sprintf("Authentication provider to use, default is SASL that supports mechanisms: %+v", mech.ListSupported()))
	driverPort           = flag.Uint("driver_port", 0, "Port that the Mesos scheduler driver process should listen on. Defaults to 0 (ephemeral).")
	hostnameOverride     = flag.String("hostname_override", "", "If non-empty, will use this string as identification instead of the actual hostname.")
)

func init() {
	flag.Var(&address, "address", "The IP address on to serve on (set to 0.0.0.0 for all interfaces). Default 127.0.0.1.")
	flag.Var(&etcdServerList, "etcd_servers", "List of etcd servers to watch (http://ip:port), comma separated. Mutually exclusive with -etcd_config")
	flag.Var(&apiServerList, "api_servers", "List of Kubernetes API servers to publish events to. (ip:port), comma separated.")
}

// returns (downloadURI, basename(path))
func serveExecutorArtifact(path string) (*string, string) {
	serveFile := func(pattern string, filename string) {
		http.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, filename)
		})
	}

	// Create base path (http://foobar:5000/<base>)
	pathSplit := strings.Split(path, "/")
	var base string
	if len(pathSplit) > 0 {
		base = pathSplit[len(pathSplit)-1]
	} else {
		base = path
	}
	serveFile("/"+base, path)

	hostURI := fmt.Sprintf("http://%s:%d/%s", address.String(), *port, base)
	log.V(2).Infof("Hosting artifact '%s' at '%s'", path, hostURI)

	return &hostURI, base
}

func prepareExecutorInfo() *mesos.ExecutorInfo {
	executorUris := []*mesos.CommandInfo_URI{}
	uri, proxyCmd := serveExecutorArtifact(*proxyPath)
	executorUris = append(executorUris, &mesos.CommandInfo_URI{Value: uri, Executable: proto.Bool(true)})
	uri, executorCmd := serveExecutorArtifact(*executorPath)
	executorUris = append(executorUris, &mesos.CommandInfo_URI{Value: uri, Executable: proto.Bool(true)})

	//TODO(jdef): provide some way (env var?) for user's to customize executor config
	//TODO(jdef): set -hostname_override and -address to 127.0.0.1 if `address` is 127.0.0.1
	apiServerArgs := strings.Join(apiServerList, ",")

	// propagate log verbosity from the scheduler to the executor,
	// but not to the proxy because it's too noisy already
	logv := flag.Lookup("v").Value.String()

	executorCommand := fmt.Sprintf("./%s -v=%s -allow_privileged=%t -api_servers=%s -proxy_exec=./%s",
		executorCmd, logv, *allowPrivileged, apiServerArgs, proxyCmd)

	if *executorBindall {
		executorCommand = fmt.Sprintf("%s -hostname_override=0.0.0.0 -address=0.0.0.0", executorCommand)
	}

	if *executorProxyBindall {
		executorCommand = fmt.Sprintf("%s -proxy_bindall=%v", executorCommand, *executorProxyBindall)
	}

	if *executorRunProxy {
		executorCommand = fmt.Sprintf("%s -run_proxy=%v", executorCommand, *executorRunProxy)
	}

	if len(etcdServerList) > 0 {
		etcdServerArguments := strings.Join(etcdServerList, ",")
		executorCommand = fmt.Sprintf("%s -etcd_servers=%s", executorCommand, etcdServerArguments)
	} else {
		uri, basename := serveExecutorArtifact(*etcdConfigFile)
		executorUris = append(executorUris, &mesos.CommandInfo_URI{Value: uri})
		executorCommand = fmt.Sprintf("%s -etcd_config=./%s", executorCommand, basename)
	}

	if *authPath != "" {
		uri, basename := serveExecutorArtifact(*authPath)
		executorUris = append(executorUris, &mesos.CommandInfo_URI{Value: uri})
		executorCommand = fmt.Sprintf("%s -auth_path=%s", executorCommand, basename)
	}

	// Create mesos scheduler driver.
	return &mesos.ExecutorInfo{
		ExecutorId: &mesos.ExecutorID{Value: proto.String(config.DefaultInfoID)},
		Command: &mesos.CommandInfo{
			Value: proto.String(executorCommand),
			Uris:  executorUris,
		},
		Name:   proto.String(config.DefaultInfoName),
		Source: proto.String(config.DefaultInfoSource),
	}
}

// hacked in from kubelet
func getApiserverClient() (*client.Client, error) {
	clientConfig := client.Config{}
	if *authPath != "" {
		authInfo, err := clientauth.LoadFromFile(*authPath)
		if err != nil {
			return nil, err
		}
		clientConfig, err = authInfo.MergeWithConfig(clientConfig)
		if err != nil {
			return nil, err
		}
	}
	// TODO(k8s): adapt client to support LB over several servers
	if len(apiServerList) > 1 {
		log.Infof("Mulitple api servers specified.  Picking first one")
	}
	clientConfig.Host = apiServerList[0]
	if c, err := client.New(&clientConfig); err != nil {
		return nil, err
	} else {
		return c, nil
	}
}

// Copied from cmd/apiserver.go
func main() {
	flag.Parse()
	util.InitLogs()
	defer util.FlushLogs()

	verflag.PrintAndExitIfRequested()

	// we'll need this for the kubelet-executor
	/*
		if (*etcdConfigFile != "" && len(etcdServerList) != 0) || (*etcdConfigFile == "" && len(etcdServerList) == 0) {
			log.Fatalf("specify either -etcd_servers or -etcd_config")
		}
	*/
	etcdClient := kubelet.EtcdClientOrDie(etcdServerList, *etcdConfigFile)
	if etcdClient == nil {
		log.Fatalf("specify either -etcd_servers or -etcd_config")
	}

	if len(apiServerList) < 1 {
		log.Fatal("No api servers specified.")
	}

	client, err := getApiserverClient()
	if err != nil {
		log.Fatalf("Unable to make apiserver client: %v", err)
	}

	// Send events to APIserver if there is a client.
	record.StartRecording(client.Events(""), api.EventSource{Component: "scheduler"})

	// Create mesos scheduler driver.
	executor := prepareExecutorInfo()
	mesosPodScheduler := scheduler.New(executor, scheduler.FCFSScheduleFunc, client, etcdClient)
	info, cred, err := buildFrameworkInfo(etcdClient)
	if err != nil {
		log.Fatalf("Misconfigured mesos framework: %v", err)
	}
	masterUri := kmcloud.MasterURI()
	dconfig := bindings.DriverConfig{
		Scheduler:        mesosPodScheduler,
		Framework:        info,
		Master:           masterUri,
		Credential:       cred,
		BindingAddress:   net.IP(address),
		BindingPort:      uint16(*driverPort),
		HostnameOverride: *hostnameOverride,
		WithAuthContext: func(ctx context.Context) context.Context {
			ctx = auth.WithLoginProvider(ctx, *authProvider)
			ctx = sasl.WithBindingAddress(ctx, net.IP(address))
			return ctx
		},
	}
	driver, err := bindings.NewMesosSchedulerDriver(dconfig)
	if err != nil {
		log.Fatalf("failed to create mesos scheduler driver: %v", err)
	}

	pluginStart := make(chan struct{})
	kpl := scheduler.NewPlugin(mesosPodScheduler.NewPluginConfig(pluginStart))
	mesosPodScheduler.Init(driver, kpl)

	if st, err := driver.Start(); err == nil {
		if st != mesos.Status_DRIVER_RUNNING {
			log.Fatalf("Scheduler driver failed to start, has status: %v", st)
		}
		go func() {
			if st, err = driver.Join(); err != nil {
				log.Fatal(err)
			} else if st != mesos.Status_DRIVER_RUNNING {
				log.Fatalf("Scheduler driver failed to join, has status: %v", st)
			} else {
				log.Fatalf("Driver stopped, aborting scheduler") //TODO(jdef) should probably exit(0) here?
			}
		}()
	} else {
		log.Fatalf("Failed to start driver: %v", err)
	}

	//TODO(jdef) we need real task reconciliation at some point
	log.V(1).Info("Clearing old pods from the registry")
	clearOldPods(client)

	go util.Forever(func() {
		log.V(1).Info("Starting HTTP interface")
		log.Error(http.ListenAndServe(net.JoinHostPort(address.String(), strconv.Itoa(*port)), nil))
	}, 5*time.Second)

	log.V(1).Info("Spinning up scheduling loop")
	close(pluginStart) // signal the plugin to spin up its background procs
	kpl.Run()

	select {}
}

func buildFrameworkInfo(client tools.EtcdClient) (info *mesos.FrameworkInfo, cred *mesos.Credential, err error) {

	var frameworkId *mesos.FrameworkID
	var failover *float64

	if *checkpoint {
		response, err := client.Get(meta.FrameworkIDKey, false, false)
		if err != nil {
			if !tools.IsEtcdNotFound(err) {
				log.Fatal(err)
			}
		} else if response.Node.Value != "" {
			log.Infof("configuring FrameworkInfo with Id found in etcd: '%s'", response.Node.Value)
			frameworkId = mutil.NewFrameworkID(response.Node.Value)
		}
		failover = proto.Float64(*failoverTimeout)
	}

	username, err := getUsername()
	if err != nil {
		return nil, nil, err
	}
	log.V(2).Infof("Framework configured with mesos user %v", username)
	info = &mesos.FrameworkInfo{
		Name:            proto.String(sconfig.DefaultInfoName),
		User:            proto.String(username),
		Checkpoint:      proto.Bool(*checkpoint),
		Id:              frameworkId,
		FailoverTimeout: failover,
	}
	if *mesosRole != "" {
		info.Role = proto.String(*mesosRole)
	}
	if *mesosAuthPrincipal != "" {
		info.Principal = proto.String(*mesosAuthPrincipal)
		if *mesosAuthSecretFile == "" {
			return nil, nil, errors.New("authentication principal specified without the required credentials file")
		}
		secret, err := ioutil.ReadFile(*mesosAuthSecretFile)
		if err != nil {
			return nil, nil, err
		}
		cred = &mesos.Credential{
			Principal: proto.String(*mesosAuthPrincipal),
			Secret:    secret,
		}
	}
	return
}

func getUsername() (username string, err error) {
	username = *mesosUser
	if username == "" {
		if u, err := user.Current(); err == nil {
			username = u.Username
			if username == "" {
				username = defaultMesosUser
			}
		}
	}
	return
}

func clearOldPods(c *client.Client) {
	ctx := api.NewDefaultContext()
	podList, err := c.Pods(api.NamespaceValue(ctx)).List(labels.Everything())
	if err != nil {
		log.Warningf("failed to clear pod registry, madness may ensue: %v", err)
		return
	}
	for _, pod := range podList.Items {
		podName := pod.Name
		if podName == "" {
			podName = string(pod.UID)
			if podName == "" {
				log.Warningf("failed to delete pod, it has no Name or UID: '%+v'", pod)
				continue
			}
		}
		err := c.Pods(pod.Namespace).Delete(podName)
		if err != nil {
			log.Warningf("failed to delete pod '%v': %v", podName, err)
		}
	}
}
