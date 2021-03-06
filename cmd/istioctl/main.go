// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ghodss/yaml"
	"github.com/golang/glog"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/spf13/cobra"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/pkg/api"

	"istio.io/pilot/apiserver"
	"istio.io/pilot/client/proxy"
	"istio.io/pilot/cmd"
	"istio.io/pilot/cmd/version"
	"istio.io/pilot/model"
	"istio.io/pilot/platform/kube"
)

const (
	// TODO istio/istio e2e integration test needs to be updated to
	// use --istioNamespace. Temporarily default to empty string so we
	// can detect and use --namespace.
	defaultIstioNamespace = "" // istio-system?
)

type k8sRESTRequester struct {
	namespace string
	service   string
	client    *kube.Client
}

// Request wraps Kubernetes specific requester to provide the proper
// namespace and service names.a
func (rr *k8sRESTRequester) Request(method, path string, inBody []byte) (int, []byte, error) {
	return rr.client.Request(rr.namespace, rr.service, method, path, inBody)
}

func kubeClientFromConfig(kubeconfig string) (*kube.Client, error) {
	if kubeconfig == "" {
		if v := os.Getenv("KUBECONFIG"); v != "" {
			glog.V(2).Infof("Setting configuration from KUBECONFIG environment variable")
			kubeconfig = v
		}
	}

	c, err := kube.NewClient(kubeconfig, model.IstioConfigTypes, istioNamespace)
	if err != nil && kubeconfig == "" {
		// If no configuration was specified, and the platform
		// client failed, try again using ~/.kube/config
		c, err = kube.NewClient(os.Getenv("HOME")+"/.kube/config", model.IstioConfigTypes, istioNamespace)
	}
	if err != nil {
		return nil, multierror.Prefix(err, "failed to connect to Kubernetes API.")
	}
	return c, nil
}

var (
	namespace             string
	istioNamespace        string
	apiClient             proxy.Client
	istioConfigAPIService string
	useKubeRequester      bool

	// input file name
	file string

	// output format (yaml or short)
	outputFormat string

	key    proxy.Key
	schema model.ProtoSchema

	rootCmd = &cobra.Command{
		Use:               "istioctl",
		Short:             "Istio control interface",
		SilenceUsage:      true,
		DisableAutoGenTag: true,
		Long: fmt.Sprintf(`
Istio configuration command line utility.

Create, list, modify, and delete configuration resources in the Istio
system.

Available routing and traffic management configuration types:

	%v

See http://istio.io/docs/reference for an overview of routing rules
and destination policies.

More information on the mixer API configuration can be found under the
istioctl mixer command documentation.
`, model.IstioConfigTypes.Types()),
		PersistentPreRunE: func(*cobra.Command, []string) error {
			var err error
			client, err = kubeClientFromConfig(kubeconfig)
			if err != nil {
				return err
			}

			if useKubeRequester {
				// TODO temporarily use --namespace instead of
				// --istioNamespace for RESTRequester namespace until
				// istio/istio e2e tests are updated.
				if istioNamespace == "" {
					istioNamespace = namespace
				}
				apiClient = proxy.NewConfigClient(&k8sRESTRequester{
					client:    client,
					namespace: istioNamespace,
					service:   istioConfigAPIService,
				})
			} else {
				apiClient = proxy.NewConfigClient(&proxy.BasicHTTPRequester{
					BaseURL: istioConfigAPIService,
					Client:  &http.Client{Timeout: 60 * time.Second},
					Version: kube.IstioResourceVersion,
				})
			}

			config = client

			return err
		},
	}

	postCmd = &cobra.Command{
		Use:   "create",
		Short: "Create policies and rules",
		Example: `
istioctl create -f example-routing.yaml
`,
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) != 0 {
				c.Println(c.UsageString())
				return fmt.Errorf("create takes no arguments")
			}
			varr, err := readInputs()
			if err != nil {
				return err
			}
			if len(varr) == 0 {
				return errors.New("nothing to create")
			}
			for _, config := range varr {
				if err = setup(config.Type, config.Name); err != nil {
					return err
				}
				err = apiClient.AddConfig(key, config)
				if err != nil {
					return err
				}
				fmt.Printf("Created config: %v %v\n", config.Type, config.Name)
			}

			return nil
		},
	}

	putCmd = &cobra.Command{
		Use:   "replace",
		Short: "Replace existing policies and rules",
		Example: `
istioctl replace -f example-routing.yaml
`,
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) != 0 {
				c.Println(c.UsageString())
				return fmt.Errorf("replace takes no arguments")
			}
			varr, err := readInputs()
			if err != nil {
				return err
			}
			if len(varr) == 0 {
				return errors.New("nothing to replace")
			}
			for _, config := range varr {
				if err = setup(config.Type, config.Name); err != nil {
					return err
				}
				err = apiClient.UpdateConfig(key, config)
				if err != nil {
					return err
				}
				fmt.Printf("Updated config: %v %v\n", config.Type, config.Name)
			}

			return nil
		},
	}

	getCmd = &cobra.Command{
		Use:   "get <type> [<name>]",
		Short: "Retrieve policies and rules",
		Example: `
# List all route rules
istioctl get route-rules

# List all destination policies
istioctl get destination-policies

# Get a specific rule named productpage-default
istioctl get route-rule productpage-default
`,
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) < 1 {
				c.Println(c.UsageString())
				return fmt.Errorf("specify the type of resource to get. Types are %v",
					strings.Join(model.IstioConfigTypes.Types(), ", "))
			}

			if len(args) > 1 {
				if err := setup(args[0], args[1]); err != nil {
					c.Println(c.UsageString())
					return err
				}
				glog.V(2).Infof("Getting single config with key: %+v", key)
				config, err := apiClient.GetConfig(key)
				if err != nil {
					return err
				}
				cSpecBytes, err := json.Marshal(config.Spec)
				if err != nil {
					return err
				}
				out, err := yaml.JSONToYAML(cSpecBytes)
				if err != nil {
					return err
				}
				fmt.Print(string(out))
			} else {
				if err := setup(args[0], ""); err != nil {
					c.Println(c.UsageString())
					return err
				}
				glog.V(2).Infof("Getting multiple configs of kind %v in namespace %v", key.Kind, key.Namespace)
				configList, err := apiClient.ListConfig(key.Kind, key.Namespace)
				if err != nil {
					return err
				}
				if len(configList) == 0 {
					fmt.Fprintf(os.Stderr, "No resources found.\n")
					return nil
				}

				var outputters = map[string](func([]apiserver.Config) error){
					"yaml":  printYamlOutput,
					"short": printShortOutput,
				}
				if outputFunc, ok := outputters[outputFormat]; ok {
					if err := outputFunc(configList); err != nil {
						return err
					}
				} else {
					return fmt.Errorf("unknown output format %v. Types are yaml|short", outputFormat)
				}

			}

			return nil
		},
	}

	deleteCmd = &cobra.Command{
		Use:   "delete <type> <name> [<name2> ... <nameN>]",
		Short: "Delete policies or rules",
		Example: `
# Delete a rule using the definition in example-routing.yaml.
istioctl delete -f example-routing.yaml

# Delete the rule productpage-default
istioctl delete route-rule productpage-default
`,
		RunE: func(c *cobra.Command, args []string) error {
			// If we did not receive a file option, get names of resources to delete from command line
			if file == "" {
				if len(args) < 2 {
					c.Println(c.UsageString())
					return fmt.Errorf("provide configuration type and name or -f option")
				}
				var errs error
				for i := 1; i < len(args); i++ {
					if err := setup(args[0], args[i]); err != nil {
						// If the user specified an invalid rule kind on the CLI,
						// don't keep processing -- it's probably a typo.
						return err
					}
					if err := apiClient.DeleteConfig(key); err != nil {
						errs = multierror.Append(errs,
							fmt.Errorf("cannot delete %s: %v", args[i], err))
					} else {
						fmt.Printf("Deleted config: %v %v\n", args[0], args[i])
					}
				}
				return errs
			}

			// As we did get a file option, make sure the command line did not include any resources to delete
			if len(args) != 0 {
				c.Println(c.UsageString())
				return fmt.Errorf("delete takes no arguments when the file option is used")
			}
			varr, err := readInputs()
			if err != nil {
				return err
			}
			if len(varr) == 0 {
				return errors.New("nothing to delete")
			}
			var errs error
			for _, v := range varr {
				if err = setup(v.Type, v.Name); err != nil {
					errs = multierror.Append(errs, err)
				} else {
					if err = apiClient.DeleteConfig(key); err != nil {
						errs = multierror.Append(errs, fmt.Errorf("cannot delete %s: %v", v.Name, err))
					} else {
						fmt.Printf("Deleted config: %v %v\n", v.Type, v.Name)
					}
				}
			}

			return errs
		},
	}

	apiVersionCmd = &cobra.Command{
		Use:   "version",
		Short: "Display version information",
		RunE: func(c *cobra.Command, args []string) error {
			fmt.Printf("istioctl version:\n\n")
			printInfo(version.Info, true)
			fmt.Printf("\n\n")

			fmt.Printf("apiserver version:\n\n")
			apiserverVersion, err := apiClient.Version()
			if err != nil {
				return err
			}
			printInfo(*apiserverVersion, false)

			return nil
		},
	}
)

func init() {
	rootCmd.PersistentFlags().StringVarP(&kubeconfig, "kubeconfig", "c", "",
		"Use a Kubernetes configuration file instead of in-cluster configuration")
	rootCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", api.NamespaceDefault,
		"Select a Kubernetes namespace")

	rootCmd.PersistentFlags().StringVar(&istioNamespace, "istioNamespace", defaultIstioNamespace,
		"Namespace where Istio system resides")
	// TODO hide istioNamespace until Istio can be installed in a
	// separate namespace from that which it manages.
	rootCmd.PersistentFlags().MarkHidden("istioNamespace") // nolint: errcheck

	rootCmd.PersistentFlags().StringVar(&istioConfigAPIService,
		"configAPIService", "istio-pilot:8081",
		"Name of Istio config service. When --kube=false this sets the address of the config service")
	rootCmd.PersistentFlags().BoolVar(&useKubeRequester, "kube", true,
		"Use Kubernetes client to send API requests to the config service")

	postCmd.PersistentFlags().StringVarP(&file, "file", "f", "",
		"Input file with the content of the configuration objects (if not set, command reads from the standard input)")
	putCmd.PersistentFlags().AddFlag(postCmd.PersistentFlags().Lookup("file"))
	deleteCmd.PersistentFlags().AddFlag(postCmd.PersistentFlags().Lookup("file"))

	getCmd.PersistentFlags().StringVarP(&outputFormat, "output", "o", "short",
		"Output format. One of:yaml|short")

	cmd.AddFlags(rootCmd)

	rootCmd.AddCommand(postCmd)
	rootCmd.AddCommand(putCmd)
	rootCmd.AddCommand(getCmd)
	rootCmd.AddCommand(deleteCmd)
	rootCmd.AddCommand(apiVersionCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(-1)
	}
}

// Set the schema, key, and namespace
// The schema is based on the kind (for example "route-rule" or "destination-policy")
// name represents the name of an instance
func setup(kind, name string) error {
	var singularForm = map[string]string{
		"route-rules":          "route-rule",
		"destination-policies": "destination-policy",
	}
	if singular, ok := singularForm[kind]; ok {
		kind = singular
	}

	// set proto schema
	var ok bool
	schema, ok = model.IstioConfigTypes.GetByType(kind)
	if !ok {
		return fmt.Errorf("Istio doesn't have configuration type %s, the types are %v",
			kind, strings.Join(model.IstioConfigTypes.Types(), ", "))
	}

	// set the config key
	key = proxy.Key{
		Kind:      kind,
		Name:      name,
		Namespace: namespace,
	}

	return nil
}

// readInputs reads multiple documents from the input and checks with the schema
func readInputs() ([]apiserver.Config, error) {

	var reader io.Reader
	var err error

	if file == "" {
		reader = os.Stdin
	} else {
		reader, err = os.Open(file)
		if err != nil {
			return nil, err
		}
	}

	var varr []apiserver.Config

	// We store route-rules as a YaML stream; there may be more than one decoder.
	yamlDecoder := kubeyaml.NewYAMLOrJSONDecoder(reader, 512*1024)
	for {
		v := apiserver.Config{}
		err = yamlDecoder.Decode(&v)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("cannot parse proto message: %v", err)
		}
		varr = append(varr, v)
	}

	return varr, nil
}

// Print a simple list of names
func printShortOutput(configList []apiserver.Config) error {
	for _, c := range configList {
		fmt.Printf("%v\n", c.Name)
	}

	return nil
}

// Print as YAML
func printYamlOutput(configList []apiserver.Config) error {
	var retVal error

	for _, c := range configList {
		cSpecBytes, err := json.Marshal(c.Spec)
		if err != nil {
			retVal = multierror.Append(retVal, err)
		}
		out, err := yaml.JSONToYAML(cSpecBytes)
		if err != nil {
			retVal = multierror.Append(retVal, err)
		}
		if err != nil {
			retVal = multierror.Append(retVal, err)
		} else {
			fmt.Printf("type: %s\n", c.Type)
			fmt.Printf("name: %s\n", c.Name)
			fmt.Printf("namespace: %s\n", namespace)
			fmt.Println("spec:")
			lines := strings.Split(string(out), "\n")
			for _, line := range lines {
				if line != "" {
					fmt.Printf("  %s\n", line)
				}
			}
		}
		fmt.Println("---")
	}

	return retVal
}

func printInfo(info version.BuildInfo, showKubeInjectInfo bool) {
	fmt.Printf("Version: %v\n", info.Version)
	fmt.Printf("GitRevision: %v\n", info.GitRevision)
	fmt.Printf("GitBranch: %v\n", info.GitBranch)
	fmt.Printf("User: %v@%v\n", info.User, info.Host)
	fmt.Printf("GolangVersion: %v\n", info.GolangVersion)

	if showKubeInjectInfo {
		fmt.Printf("KubeInjectHub: %v\n", version.KubeInjectHub)
		fmt.Printf("KubeInjectTag: %v\n", version.KubeInjectTag)
	}
}
