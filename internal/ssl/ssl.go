package ssl

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/ghodss/yaml"
	"github.com/kubefirst/kubefirst/configs"
	"github.com/kubefirst/kubefirst/internal/aws"
	"github.com/kubefirst/kubefirst/internal/k8s"
	"github.com/kubefirst/kubefirst/pkg"
	"github.com/spf13/viper"
	yaml2 "gopkg.in/yaml.v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

var namespaces []string

func init() {
	namespaces = []string{"argo", "atlantis", "chartmuseum", "gitlab", "vault"}
}

func getItemsToBackup(apiGroup string, apiVersion string, resourceType string, namespaces []string, jqQuery string) ([]string, error) {
	config := configs.ReadConfig()

	k8sConfig, err := clientcmd.BuildConfigFromFlags("", config.KubeConfigPath)
	if err != nil {
		return nil, fmt.Errorf("error getting k8sClient %s", err)
	}

	k8sClient := dynamic.NewForConfigOrDie(k8sConfig)

	//creating folder to store certificates backup, and continue if exists.
	if err := os.Mkdir(fmt.Sprintf("%s", config.CertsPath), os.ModePerm); err != nil {
		log.Printf("error: could not create directory %q - it must exist to continue. error is: %s", config.CertsPath, err)
	}

	var files []string
	var items []unstructured.Unstructured
	for _, namespace := range namespaces {
		if len(jqQuery) > 0 {
			log.Println("getting resources and filtering using jq")
			items, err = k8s.GetResourcesByJq(k8sClient, context.TODO(), apiGroup, apiVersion, resourceType, namespace, jqQuery)
		} else {
			log.Println("getting resources")
			items, err = k8s.GetResourcesDynamically(k8sClient, context.TODO(), apiGroup, apiVersion, resourceType, namespace)
		}

		if err != nil {
			return nil, fmt.Errorf("error getting resources from k8s: %s", err)
		}

		for _, item := range items {
			jsonObj, err := item.MarshalJSON()
			if err != nil {
				return nil, fmt.Errorf("error converting object on json: %s", err)
			}
			//yamlObj, err := yaml.JSONToYAML(jsonObj)
			yamlObj, err := yaml.JSONToYAML(jsonObj)
			if err != nil {
				return nil, fmt.Errorf("error converting object from json to yaml: %s", err)
			}

			fileName := fmt.Sprintf("%s.%s", item.GetName(), "yaml")
			//TODO: test if kubeconfigpath is the correct value to write the files together another k1rst files
			fullFileName := filepath.Join(config.CertsPath, fileName)
			err = pkg.CreateFile(fullFileName, yamlObj)
			if err != nil {
				return nil, err
			}
			files = append(files, fullFileName)
		}
	}

	return files, nil
}

//func GetBackupCertificates(apiGroup string, apiVersion string, resourceTypes []string, namespace string) ([]string, error) {
// GetBackupCertificates create a backup of Certificates on AWS S3 in yaml files
func GetBackupCertificates() (string, error) {
	config := configs.ReadConfig()

	log.Println("GetBackupCertificates called")
	bucketName := fmt.Sprintf("k1-%s", viper.GetString("aws.hostedzonename"))
	//path := "cert-manager"
	aws.CreateBucket(false, bucketName)

	log.Println("getting certificates")
	certificates, err := getItemsToBackup("cert-manager.io", "v1", "certificates", namespaces, "")
	if err != nil {
		return "", fmt.Errorf("erro: %s", err)
	}
	for _, cert := range certificates {
		fullPath := strings.Replace(cert, config.CertsPath, "/certs", 1)
		log.Println(fullPath)
		aws.UploadFile(bucketName, fullPath, cert)
	}

	log.Println("getting secrets")
	query := ".metadata.annotations[\"cert-manager.io/issuer-kind\"] == \"ClusterIssuer\""
	secrets, err := getItemsToBackup("", "v1", "secrets", namespaces, query)
	if err != nil {
		return "", fmt.Errorf("erro: %s", err)
	}
	for _, secret := range secrets {
		fullPath := strings.Replace(secret, config.CertsPath, "/secrets", 1)
		log.Println(fullPath)
		aws.UploadFile(bucketName, fullPath, secret)
	}

	emptyNS := []string{""}
	log.Println("getting clusterissuers")
	clusterIssuers, err := getItemsToBackup("cert-manager.io", "v1", "clusterissuers", emptyNS, "")
	if err != nil {
		return "", fmt.Errorf("erro: %s", err)
	}
	for _, clusterissuer := range clusterIssuers {
		fullPath := strings.Replace(clusterissuer, config.CertsPath, "/clusterissuers", 1)
		log.Println(fullPath)
		aws.UploadFile(bucketName, fullPath, clusterissuer)
	}

	return "Backuped Cert-Manager resources finished successfully!", nil
}

func RestoreSSL() error {
	config := configs.ReadConfig()

	for _, ns := range namespaces {
		_, _, err := pkg.ExecShellReturnStrings(config.KubectlClientPath, "--kubeconfig", config.KubeConfigPath, "create", "ns", ns)
		if err != nil {
			log.Print("failed to create ns: %s, assuming that exists...", err)
		}
	}
	bucketName := fmt.Sprintf("k1-%s", viper.GetString("aws.hostedzonename"))
	aws.DownloadBucket(bucketName, config.CertsPath)
	//! We need apply secrets firstly than other resources, accordingly with cert-manager docs
	pathsRestored := []string{"secrets", "certs", "clusterissuers"}
	for _, path := range pathsRestored {
		log.Print(path)
		//clean yaml
		//TODO filter yaml extension
		files, err := ioutil.ReadDir(fmt.Sprintf("%s/%s", filepath.Join(config.CertsPath, path), "/"))
		if err != nil {
			return fmt.Errorf("erro: %s", err)
		}

		for _, f := range files {
			log.Println(f.Name())
			pathyaml := fmt.Sprintf("%s/%s", filepath.Join(config.CertsPath, path), f.Name())

			yfile, err := ioutil.ReadFile(pathyaml)

			if err != nil {
				return fmt.Errorf("erro: %s", err)
			}

			data := make(map[interface{}]interface{})

			err = yaml2.Unmarshal(yfile, &data)

			if err != nil {
				return fmt.Errorf("erro: %s", err)
			}

			metadataMap := data["metadata"].(map[interface{}]interface{})
			delete(metadataMap, "resourceVersion")
			delete(metadataMap, "uid")
			delete(metadataMap, "creationTimestamp")
			delete(metadataMap, "managedFields")
			data["metadata"] = metadataMap
			dataCleaned, err := yaml2.Marshal(&data)

			if err != nil {
				return fmt.Errorf("erro: %s", err)
			}

			err = ioutil.WriteFile(fmt.Sprintf("%s%s", pathyaml, ".clean"), dataCleaned, 0644)

			if err != nil {
				return fmt.Errorf("erro: %s", err)
			}

			log.Println("yaml cleaned written")
		}

		log.Printf("applying the folder: %s", path)
		_, _, err = pkg.ExecShellReturnStrings(config.KubectlClientPath, "--kubeconfig", config.KubeConfigPath, "apply", "-f", filepath.Join(config.CertsPath, path))
		if err != nil {
			log.Printf("failed to apply %s: %s, assuming that exists...", path, err)
		}
	}
	return nil
}
