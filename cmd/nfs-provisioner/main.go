/*
Copyright 2016 The Kubernetes Authors.

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

package main

import (
	"flag"
	"github.com/kubernetes-sigs/nfs-ganesha-server-and-external-provisioner/pkg/mounter"
	"github.com/kubernetes-sigs/nfs-ganesha-server-and-external-provisioner/pkg/s3"
	"os"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/kubernetes-sigs/nfs-ganesha-server-and-external-provisioner/pkg/server"
	vol "github.com/kubernetes-sigs/nfs-ganesha-server-and-external-provisioner/pkg/volume"
	"github.com/kubernetes-sigs/sig-storage-lib-external-provisioner/controller"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	provisioner         = flag.String("provisioner", "example.com/nfs", "Name of the provisioner. The provisioner will only provision volumes for claims that request a StorageClass with a provisioner field set equal to this name.")
	master              = flag.String("master", "", "Master URL to build a client config from. Either this or kubeconfig needs to be set if the provisioner is being run out of cluster.")
	kubeconfig          = flag.String("kubeconfig", "", "Absolute path to the kubeconfig file. Either this or master needs to be set if the provisioner is being run out of cluster.")
	runServer           = flag.Bool("run-server", true, "If the provisioner is responsible for running the NFS server, i.e. starting and stopping NFS Ganesha. Default true.")
	useGanesha          = flag.Bool("use-ganesha", true, "If the provisioner will create volumes using NFS Ganesha (D-Bus method calls) as opposed to using the kernel NFS server ('exportfs'). If run-server is true, this must be true. Default true.")
	gracePeriod         = flag.Uint("grace-period", 90, "NFS Ganesha grace period to use in seconds, from 0-180. If the server is not expected to survive restarts, i.e. it is running as a pod & its export directory is not persisted, this can be set to 0. Can only be set if both run-server and use-ganesha are true. Default 90.")
	enableXfsQuota      = flag.Bool("enable-xfs-quota", false, "If the provisioner will set xfs quotas for each volume it provisions. Requires that the directory it creates volumes in ('/export') is xfs mounted with option prjquota/pquota, and that it has the privilege to run xfs_quota. Default false.")
	serverHostname      = flag.String("server-hostname", "", "The hostname for the NFS server to export from. Only applicable when running out-of-cluster i.e. it can only be set if either master or kubeconfig are set. If unset, the first IP output by `hostname -i` is used.")
	exportSubnet        = flag.String("export-subnet", "*", "Subnet for NFS export to allow mount only from")
	maxExports          = flag.Int("max-exports", -1, "The maximum number of volumes to be exported by this provisioner. New claims will be ignored once this limit has been reached. A negative value is interpreted as 'unlimited'. Default -1.")
	fsidDevice          = flag.Bool("device-based-fsids", true, "If file system handles created by NFS Ganesha should be based on major/minor device IDs of the backing storage volume ('/export'). Default true.")
	leaderElection      = flag.Bool("leader-elect", false, "Start a leader election client and gain leadership before executing the main loop. Enable this when running replicated components for high availability. Default false.")
	useS3StorageBackend = flag.Bool("use-s3-backend", false, "Use S3 as the storage backend")
	s3BucketName        = flag.String("s3-bucket-name", "", "S3 Bucket Name")
	s3RootDir           = flag.String("s3-root-dir", "", "S3 Root Directory")
	s3Region            = flag.String("s3-region", "", "S3 Region")
	s3Endpoint          = flag.String("s3-endpoint", "", "S3 Endpoint")
	s3TargetMountDir    = flag.String("s3-target-mount-dir", "/mnt/s3", "Target Directory to mount the S3 Bucket and Root Dir")
)

const (
	exportDir     = "/export"
	ganeshaLog    = "/export/ganesha.log"
	ganeshaPid    = "/var/run/ganesha.pid"
	ganeshaConfig = "/export/vfs.conf"
)

func main() {
	flag.Set("logtostderr", "true")
	flag.Parse()

	if errs := validateProvisioner(*provisioner, field.NewPath("provisioner")); len(errs) != 0 {
		glog.Fatalf("Invalid provisioner specified: %v", errs)
	}
	glog.Infof("Provisioner %s specified", *provisioner)

	if *runServer && !*useGanesha {
		glog.Fatalf("Invalid flags specified: if run-server is true, use-ganesha must also be true.")
	}

	if *useGanesha && *exportSubnet != "*" {
		glog.Warningf("If use-ganesha is true, there is no effect on export-subnet.")
	}

	if *gracePeriod != 90 && (!*runServer || !*useGanesha) {
		glog.Fatalf("Invalid flags specified: custom grace period can only be set if both run-server and use-ganesha are true.")
	} else if *gracePeriod > 180 && *runServer && *useGanesha {
		glog.Fatalf("Invalid flags specified: custom grace period must be in the range 0-180")
	}

	// Create the client according to whether we are running in or out-of-cluster
	outOfCluster := *master != "" || *kubeconfig != ""

	if !outOfCluster && *serverHostname != "" {
		glog.Fatalf("Invalid flags specified: if server-hostname is set, either master or kube-config must also be set.")
	}

	if *useS3StorageBackend {
		glog.Infof("Using S3 Backend")
		aki := os.Getenv("AWS_ACCESS_KEY_ID")
		sak := os.Getenv("AWS_SECRET_ACCESS_KEY")

		if *s3BucketName == "" {
			glog.Fatalf("Invalid flags specified: if use-s3-backend is set, s3-bucket-name must also be set.")
		}
		if *s3Endpoint == "" {
			glog.Fatalf("Invalid flags specified: if use-s3-backend is set, s3-endpoint must also be set.")
		}

		meta := &s3.FSMeta{
			BucketName: *s3BucketName,
			Prefix:     *s3RootDir,
			Mounter:    "rclone",
			FSPath:     "",
		}

		glog.Infof("Creating S3 Client")
		s3, err := s3.NewClientFromEnv(aki, sak, *s3Region, *s3Endpoint)
		if err != nil {
			glog.Fatalf("failed to initialize S3 client: %s", err)
		}

		exists, err := s3.BucketExists(*s3BucketName)
		if err != nil {
			glog.Fatalf("failed to check if bucket %s exists: %v", *s3BucketName, err)
		}

		if exists {
			// get meta, ignore errors as it could just mean meta does not exist yet
			_, err := s3.GetFSMeta(*s3BucketName, *s3RootDir)
			if err != nil {
			}
		} else {
			if err = s3.CreateBucket(*s3BucketName); err != nil {
				glog.Fatalf("failed to create bucket %s: %v", *s3BucketName, err)
			}
		}


		if err = s3.CreatePrefix(*s3BucketName, *s3RootDir); err != nil {
			glog.Fatalf("failed to create prefix %s: %v", *s3RootDir, err)
		}

		if err := s3.SetFSMeta(meta); err != nil {
			glog.Fatalf("error setting bucket metadata: %w", err)
		}

		meta, err = s3.GetFSMeta(*s3BucketName, *s3RootDir)
		if err != nil {
			glog.Fatalf("failed to get S3 metadata: %s", err)
		}
		glog.Infof("Creating S3 Mounter")
		m, err := mounter.New(meta, s3.Config)
		if err != nil {
			glog.Fatalf("failed to initialize S3 backend mounter: %s", err)
		}
		if _, err := os.Stat(*s3TargetMountDir); os.IsNotExist(err) {
			if err = os.MkdirAll(*s3TargetMountDir, 0755); err != nil {
				glog.Fatalf("failed to create S3 target mount dir: %s", err)
			}
		}
		glog.Infof("Mounting S3")
		if err := m.Mount(*s3TargetMountDir); err != nil {
			glog.Fatalf("failed to initialize S3 backend mount: %s", err)
		}
		glog.Infof("s3: volume %s/%s successfuly mounted to %s", *s3BucketName, *s3RootDir, *s3TargetMountDir)
	}

	if *runServer {
		glog.Infof("Setting up NFS server!")
		err := server.Setup(ganeshaConfig, *gracePeriod, *fsidDevice)
		if err != nil {
			glog.Fatalf("Error setting up NFS server: %v", err)
		}
		go func() {
			for {
				// This blocks until server exits (presumably due to an error)
				err = server.Run(ganeshaLog, ganeshaPid, ganeshaConfig)
				if err != nil {
					glog.Errorf("NFS server Exited Unexpectedly with err: %v", err)
				}

				// take a moment before trying to restart
				time.Sleep(time.Second)
			}
		}()
		// Wait for NFS server to come up before continuing provisioner process
		time.Sleep(5 * time.Second)
	}

	var config *rest.Config
	var err error
	if outOfCluster {
		config, err = clientcmd.BuildConfigFromFlags(*master, *kubeconfig)
	} else {
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		glog.Fatalf("Failed to create config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		glog.Fatalf("Failed to create client: %v", err)
	}

	// The controller needs to know what the server version is because out-of-tree
	// provisioners aren't officially supported until 1.5
	serverVersion, err := clientset.Discovery().ServerVersion()
	if err != nil {
		glog.Fatalf("Error getting server version: %v", err)
	}

	// Create the provisioner: it implements the Provisioner interface expected by
	// the controller
	nfsProvisioner := vol.NewNFSProvisioner(exportDir, clientset, outOfCluster, *useGanesha, ganeshaConfig, *enableXfsQuota, *serverHostname, *maxExports, *exportSubnet)

	// Start the provision controller which will dynamically provision NFS PVs
	pc := controller.NewProvisionController(
		clientset,
		*provisioner,
		nfsProvisioner,
		serverVersion.GitVersion,
		controller.LeaderElection(*leaderElection),
	)

	pc.Run(wait.NeverStop)

	if err := mounter.FuseUnmount(*s3TargetMountDir); err != nil {
		glog.Fatalf("failed to unmount S3 backend: %s", err)
	}
}

// validateProvisioner tests if provisioner is a valid qualified name.
// https://github.com/kubernetes/kubernetes/blob/release-1.4/pkg/apis/storage/validation/validation.go
func validateProvisioner(provisioner string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(provisioner) == 0 {
		allErrs = append(allErrs, field.Required(fldPath, provisioner))
	}
	if len(provisioner) > 0 {
		for _, msg := range validation.IsQualifiedName(strings.ToLower(provisioner)) {
			allErrs = append(allErrs, field.Invalid(fldPath, provisioner, msg))
		}
	}
	return allErrs
}
