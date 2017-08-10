package windows_utilities_tests_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"
)

func init() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.SetOutput(GinkgoWriter)
}

const BOSH_TIMEOUT = 45 * time.Minute
const GoZipFile = "go1.7.1.windows-amd64.zip"
const GolangURL = "https://storage.googleapis.com/golang/" + GoZipFile

var manifestTemplate = `
---
name: {{.DeploymentName}}

releases:
- name: {{.ReleaseName}}
  version: latest
- name: windows-utilities
  version: latest

stemcells:
- alias: windows
  os: {{.StemcellOs}}
  version: latest

update:
  canaries: 0
  canary_watch_time: 60000
  update_watch_time: 60000
  max_in_flight: 2

instance_groups:
- name: kms-host-enabled
  instances: 1
  stemcell: windows
  lifecycle: errand
  azs: [{{.AZ}}]
  vm_type: {{.VmType}}
  vm_extensions: [{{.VmExtensions}}]
  networks:
  - name: {{.Network}}
  jobs:
  - name: check_kms_host
    release: {{.ReleaseName}}
    properties:
      check_kms_host:
        host: test.test
        port: 1234
  - name: set_kms_host
    release: windows-utilities
    properties:
      set_kms_host:
        enabled: true
        host: test.test
        port: 1234
- name: kms-host-not-enabled
  instances: 1
  stemcell: windows
  lifecycle: errand
  azs: [{{.AZ}}]
  vm_type: {{.VmType}}
  vm_extensions: [{{.VmExtensions}}]
  networks:
  - name: {{.Network}}
  jobs:
  - name: check_kms_host
    release: {{.ReleaseName}}
    properties:
      check_kms_host:
        host:
        port:
  - name: set_kms_host
    release: windows-utilities
    properties:
      set_kms_host:
        enabled: false
        host: test.test
        port: 1234
- name: kms-host-enabled-with-default
  instances: 1
  stemcell: windows
  lifecycle: errand
  azs: [{{.AZ}}]
  vm_type: {{.VmType}}
  vm_extensions: [{{.VmExtensions}}]
  networks:
  - name: {{.Network}}
  jobs:
  - name: check_kms_host
    release: {{.ReleaseName}}
    properties:
      check_kms_host:
        host: test.test
        port: 1688
  - name: set_kms_host
    release: windows-utilities
    properties:
      set_kms_host:
        enabled: true
        host: test.test
        port:
`

type ManifestProperties struct {
	DeploymentName string
	ReleaseName    string
	AZ             string
	VmType         string
	VmExtensions   string
	Network        string
	StemcellOs     string
}

type Config struct {
	Bosh struct {
		CaCert       string `json:"ca_cert"`
		Client       string `json:"client"`
		ClientSecret string `json:"client_secret"`
		Target       string `json:"target"`
	} `json:"bosh"`
	Stemcellpath         string `json:"stemcell_path"`
	Windowsutilitiespath string `json:"windows_utilities_path"`
	StemcellOs           string `json:"stemcell_os"`
	Az                   string `json:"az"`
	VmType               string `json:"vm_type"`
	VmExtensions         string `json:"vm_extensions"`
	Network              string `json:"network"`
}

func NewConfig() (*Config, error) {
	configFilePath := os.Getenv("CONFIG_JSON")
	if configFilePath == "" {
		return nil, fmt.Errorf("invalid config file path: %v", configFilePath)
	}
	body, err := ioutil.ReadFile(configFilePath)
	if err != nil {
		return nil, fmt.Errorf("empty config file path: %v", configFilePath)
	}
	var config Config
	err = json.Unmarshal(body, &config)
	if err != nil {
		return nil, fmt.Errorf("unable to parse config file: %v", body)
	}
	return &config, nil
}

func (c *Config) generateManifest(deploymentName string) ([]byte, error) {
	manifestProperties := ManifestProperties{
		DeploymentName: deploymentName,
		ReleaseName:    "wuts-release",
		AZ:             c.Az,
		VmType:         c.VmType,
		VmExtensions:   c.VmExtensions,
		Network:        c.Network,
		StemcellOs:     c.StemcellOs,
	}
	templ, err := template.New("").Parse(manifestTemplate)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	err = templ.Execute(&buf, manifestProperties)
	return buf.Bytes(), err
}

type BoshCommand struct {
	DirectorIP   string
	Client       string
	ClientSecret string
	CertPath     string // Path to CA CERT file, if any
	Timeout      time.Duration
}

func NewBoshCommand(config *Config, CertPath string, duration time.Duration) *BoshCommand {
	return &BoshCommand{
		DirectorIP:   config.Bosh.Target,
		Client:       config.Bosh.Client,
		ClientSecret: config.Bosh.ClientSecret,
		CertPath:     CertPath,
		Timeout:      duration,
	}
}

func (c *BoshCommand) args(command string) []string {
	args := strings.Split(command, " ")
	args = append([]string{"-n", "-e", c.DirectorIP, "--client", c.Client, "--client-secret", c.ClientSecret}, args...)
	if c.CertPath != "" {
		args = append([]string{"--ca-cert", c.CertPath}, args...)
	}
	return args
}

func (c *BoshCommand) Run(command string) error {
	cmd := exec.Command("bosh", c.args(command)...)
	log.Printf("\nRUNNING %q\n", strings.Join(cmd.Args, " "))

	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	if err != nil {
		return err
	}
	session.Wait(c.Timeout)

	exitCode := session.ExitCode()
	if exitCode != 0 {
		var stderr []byte
		if session.Err != nil {
			stderr = session.Err.Contents()
		}
		stdout := session.Out.Contents()
		return fmt.Errorf("Non-zero exit code for cmd %q: %d\nSTDERR:\n%s\nSTDOUT:%s\n",
			strings.Join(cmd.Args, " "), exitCode, stderr, stdout)
	}
	return nil
}

func downloadGo() (string, error) {
	dirname, err := ioutil.TempDir("", "")
	if err != nil {
		return "", err
	}

	path := filepath.Join(dirname, GoZipFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0666)
	if err != nil {
		return "", err
	}
	defer f.Close()

	res, err := http.Get(GolangURL)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if _, err := io.Copy(f, res.Body); err != nil {
		return "", err
	}

	return path, nil
}

func downloadLogs(jobName string, index int) *gbytes.Buffer {
	tempDir, err := ioutil.TempDir("", "")
	Expect(err).To(Succeed())
	defer os.RemoveAll(tempDir)

	err = bosh.Run(fmt.Sprintf("-d %s logs %s/%d --dir %s", deploymentName, jobName, index, tempDir))
	Expect(err).To(Succeed())

	matches, err := filepath.Glob(filepath.Join(tempDir, fmt.Sprintf("%s.%s.%d-*.tgz", deploymentName, jobName, index)))
	Expect(err).To(Succeed())
	Expect(matches).To(HaveLen(1))

	cmd := exec.Command("tar", "xf", matches[0], "-O", fmt.Sprintf("./%s/%s/job-service-wrapper.out.log", jobName, jobName))
	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).To(Succeed())

	return session.Wait().Out
}

var (
	bosh           *BoshCommand
	deploymentName string
	manifestPath   string
	boshCertPath   string
)

var _ = Describe("Windows Utilities Release", func() {

	BeforeSuite(func() {
		config, err := NewConfig()
		Expect(err).To(Succeed())

		cert := config.Bosh.CaCert
		if cert != "" {
			certFile, err := ioutil.TempFile("", "")
			Expect(err).To(Succeed())

			_, err = certFile.Write([]byte(cert))
			Expect(err).To(Succeed())

			boshCertPath, err = filepath.Abs(certFile.Name())
			Expect(err).To(Succeed())
		}

		bosh = NewBoshCommand(config, boshCertPath, BOSH_TIMEOUT)

		bosh.Run("login")
		deploymentName = fmt.Sprintf("windows-utilities-test-%d", time.Now().UTC().Unix())

		pwd, err := os.Getwd()
		Expect(err).To(Succeed())
		Expect(os.Chdir(filepath.Join(pwd, "assets", "wuts-release"))).To(Succeed()) // push
		defer os.Chdir(pwd)                                                          // pop

		manifest, err := config.generateManifest(deploymentName)
		Expect(err).To(Succeed())
		manifestFile, err := ioutil.TempFile("", "")
		Expect(err).To(Succeed())
		_, err = manifestFile.Write(manifest)
		Expect(err).To(Succeed())
		manifestPath, err = filepath.Abs(manifestFile.Name())
		Expect(err).To(Succeed())

		// Upload wuts-release from this repository
		Expect(bosh.Run("create-release --force --timestamp-version")).To(Succeed())
		Expect(bosh.Run("upload-release")).To(Succeed())

		// Upload latest windows-utilities release
		matches, err := filepath.Glob(config.Windowsutilitiespath)
		Expect(err).To(Succeed())
		Expect(matches).To(HaveLen(1))

		Expect(bosh.Run(fmt.Sprintf("upload-release %s", matches[0]))).To(Succeed())

		// Upload latest stemcell
		matches, err = filepath.Glob(config.Stemcellpath)
		Expect(err).To(Succeed())
		Expect(matches).To(HaveLen(1))

		err = bosh.Run(fmt.Sprintf("upload-stemcell %s", matches[0]))
		if err != nil {
			//AWS takes a while to distribute the AMI across accounts
			time.Sleep(2 * time.Minute)
		}
		Expect(err).To(Succeed())
	})

	It("Enables KMS with Host and custom Port", func() {
		err := bosh.Run(fmt.Sprintf("-d %s deploy %s", deploymentName, manifestPath))
		Expect(err).To(Succeed())
		err = bosh.Run(fmt.Sprintf("-d %s run-errand kms-host-enabled", deploymentName))
		Expect(err).To(Succeed())
	})

	It("Does not enable KMS", func() {
		err := bosh.Run(fmt.Sprintf("-d %s deploy %s", deploymentName, manifestPath))
		Expect(err).To(Succeed())
		err = bosh.Run(fmt.Sprintf("-d %s run-errand kms-host-not-enabled", deploymentName))
		Expect(err).To(Succeed())
	})

	It("Enables KMS with Host and default Port", func() {
		err := bosh.Run(fmt.Sprintf("-d %s deploy %s", deploymentName, manifestPath))
		Expect(err).To(Succeed())
		err = bosh.Run(fmt.Sprintf("-d %s run-errand kms-host-enabled-with-default", deploymentName))
		Expect(err).To(Succeed())
	})

	AfterSuite(func() {
		bosh.Run(fmt.Sprintf("-d %s delete-deployment --force", deploymentName))

		bosh.Run("clean-up --all")
		if bosh.CertPath != "" {
			os.RemoveAll(bosh.CertPath)
		}
		if manifestPath != "" {
			os.RemoveAll(manifestPath)
		}
	})
})
