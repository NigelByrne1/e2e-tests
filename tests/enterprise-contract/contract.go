package enterprisecontract

import (
	"fmt"
	"os"
	"time"

	"github.com/devfile/library/v2/pkg/util"
	ecp "github.com/enterprise-contract/enterprise-contract-controller/api/v1alpha1"
	"github.com/konflux-ci/e2e-tests/pkg/clients/common"
	kubeapi "github.com/konflux-ci/e2e-tests/pkg/clients/kubernetes"
	"github.com/konflux-ci/e2e-tests/pkg/constants"
	"github.com/konflux-ci/e2e-tests/pkg/framework"
	"github.com/konflux-ci/e2e-tests/pkg/utils"
	"github.com/konflux-ci/e2e-tests/pkg/utils/contract"
	"github.com/konflux-ci/e2e-tests/pkg/utils/tekton"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	pipeline "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	corev1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

var _ = framework.EnterpriseContractSuiteDescribe("Conforma E2E tests", Label("ec"), func() {

	defer GinkgoRecover()

	var namespace string
	var kubeClient *framework.ControllerHub
	var fwk *framework.Framework

	AfterEach(framework.ReportFailure(&fwk))

	BeforeAll(func() {
		// Allow the use of a custom namespace for testing.
		namespace = os.Getenv(constants.E2E_APPLICATIONS_NAMESPACE_ENV)
		if len(namespace) > 0 {
			adminClient, err := kubeapi.NewAdminKubernetesClient()
			Expect(err).ShouldNot(HaveOccurred())
			kubeClient, err = framework.InitControllerHub(adminClient)
			Expect(err).ShouldNot(HaveOccurred())
			_, err = kubeClient.CommonController.CreateTestNamespace(namespace)
			Expect(err).ShouldNot(HaveOccurred())
		} else {
			var err error
			fwk, err = framework.NewFramework(utils.GetGeneratedNamespace(constants.TEKTON_CHAINS_E2E_USER))
			Expect(err).NotTo(HaveOccurred())
			Expect(fwk.UserNamespace).NotTo(BeNil(), "failed to create sandbox user")
			namespace = fwk.UserNamespace
			kubeClient = fwk.AsKubeAdmin
		}
	})

	Context("infrastructure is running", Label("pipeline"), func() {
		It("verifies if the chains controller is running", func() {
			err := kubeClient.CommonController.WaitForPodSelector(kubeClient.CommonController.IsPodRunning, constants.TEKTON_CHAINS_NS, "app", "tekton-chains-controller", 60, 100)
			Expect(err).NotTo(HaveOccurred())
		})

		It("verifies the signing secret is present", func() {
			timeout := time.Minute * 5
			interval := time.Second * 1

			Eventually(func() bool {
				config, err := kubeClient.CommonController.GetSecret(constants.TEKTON_CHAINS_NS, constants.TEKTON_CHAINS_SIGNING_SECRETS_NAME)
				Expect(err).NotTo(HaveOccurred())

				_, private := config.Data["cosign.key"]
				_, public := config.Data["cosign.pub"]
				_, password := config.Data["cosign.password"]

				return private && public && password
			}, timeout, interval).Should(BeTrue(), fmt.Sprintf("timed out when waiting for Tekton Chains signing secret %q to be present in %q namespace", constants.TEKTON_CHAINS_SIGNING_SECRETS_NAME, constants.TEKTON_CHAINS_NS))
		})
	})

	Context("test creating and signing an image and task", Label("pipeline"), func() {
		// Make the PipelineRun name and namespace predictable. For convenience, the name of the
		// PipelineRun that builds an image, is the same as the repository where the image is
		// pushed to.
		var buildPipelineRunName, image, imageWithDigest string
		var pipelineRunTimeout int
		var defaultECP *ecp.EnterpriseContractPolicy

		BeforeAll(func() {
			buildPipelineRunName = fmt.Sprintf("buildah-demo-%s", util.GenerateRandomString(10))
			image = fmt.Sprintf("quay.io/%s/test-images:%s", utils.GetQuayIOOrganization(), buildPipelineRunName)
			sharedSecret, err := fwk.AsKubeAdmin.CommonController.GetSecret(constants.QuayRepositorySecretNamespace, constants.QuayRepositorySecretName)
			Expect(err).ShouldNot(HaveOccurred(), fmt.Sprintf("error when getting shared secret - make sure the secret %s in %s namespace is created", constants.QuayRepositorySecretName, constants.QuayRepositorySecretNamespace))

			_, err = fwk.AsKubeAdmin.CommonController.GetSecret(namespace, constants.QuayRepositorySecretName)
			if err == nil {
				err = fwk.AsKubeAdmin.CommonController.DeleteSecret(namespace, constants.QuayRepositorySecretName)
				Expect(err).ToNot(HaveOccurred())
			} else if !k8sErrors.IsNotFound(err) {
				Expect(err).ToNot(HaveOccurred())
			}

			repositorySecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: constants.QuayRepositorySecretName, Namespace: namespace},
				Type: corev1.SecretTypeDockerConfigJson,
				Data: map[string][]byte{corev1.DockerConfigJsonKey: sharedSecret.Data[".dockerconfigjson"]}}
			_, err = fwk.AsKubeAdmin.CommonController.CreateSecret(namespace, repositorySecret)
			Expect(err).ShouldNot(HaveOccurred())
			err = fwk.AsKubeAdmin.CommonController.LinkSecretToServiceAccount(namespace, constants.QuayRepositorySecretName, constants.DefaultPipelineServiceAccount, true)
			Expect(err).ToNot(HaveOccurred())

			pipelineRunTimeout = int(time.Duration(20) * time.Minute)

			defaultECP, err = fwk.AsKubeAdmin.TektonController.GetEnterpriseContractPolicy("default", "enterprise-contract-service")
			Expect(err).NotTo(HaveOccurred())

			// Trigger a demo task to generate an image that has been signed by Tekton Chains within the context for each spec
			bundles, err := kubeClient.TektonController.NewBundles()
			Expect(err).ShouldNot(HaveOccurred())
			dockerBuildBundle := bundles.DockerBuildBundle
			Expect(dockerBuildBundle).NotTo(Equal(""), "Can't continue without a docker-build pipeline got from selector config")
			pr, err := fwk.AsKubeAdmin.TektonController.RunPipeline(tekton.BuildahDemo{Image: image, Bundle: dockerBuildBundle, Namespace: namespace, Name: buildPipelineRunName}, namespace, pipelineRunTimeout)
			Expect(err).NotTo(HaveOccurred())
			// Verify that the build task was created as expected.
			Expect(pr.ObjectMeta.Name).To(Equal(buildPipelineRunName))
			Expect(pr.ObjectMeta.Namespace).To(Equal(namespace))
			Expect(fwk.AsKubeAdmin.TektonController.WatchPipelineRun(pr.Name, namespace, pipelineRunTimeout)).To(Succeed())
			GinkgoWriter.Printf("The pipeline named %q in namespace %q succeeded\n", pr.ObjectMeta.Name, pr.ObjectMeta.Namespace)

			// The PipelineRun resource has been updated, refresh our reference.
			pr, err = fwk.AsKubeAdmin.TektonController.GetPipelineRun(pr.ObjectMeta.Name, pr.ObjectMeta.Namespace)
			Expect(err).NotTo(HaveOccurred())

			// Verify TaskRun has the type hinting required by Tekton Chains
			digest, err := fwk.AsKubeAdmin.TektonController.GetTaskRunResult(fwk.AsKubeAdmin.CommonController.KubeRest(), pr, "build-container", "IMAGE_DIGEST")
			Expect(err).NotTo(HaveOccurred())
			i, err := fwk.AsKubeAdmin.TektonController.GetTaskRunResult(fwk.AsKubeAdmin.CommonController.KubeRest(), pr, "build-container", "IMAGE_URL")
			Expect(err).NotTo(HaveOccurred())
			Expect(i).To(Equal(image))

			// Specs now have a deterministic image reference for validation \o/
			imageWithDigest = fmt.Sprintf("%s@%s", image, digest)

			GinkgoWriter.Printf("The image signed by Tekton Chains is %s\n", imageWithDigest)
		})

		It("creates signature and attestation", func() {
			err := fwk.AsKubeAdmin.TektonController.AwaitAttestationAndSignature(imageWithDigest, constants.ChainsAttestationTimeout)
			Expect(err).NotTo(
				HaveOccurred(),
				"Could not find .att or .sig ImageStreamTags within the %s timeout. "+
					"Most likely the chains-controller did not create those in time. "+
					"Look at the chains-controller logs.",
				constants.ChainsAttestationTimeout.String(),
			)
			GinkgoWriter.Printf("Cosign verify pass with .att and .sig ImageStreamTags found for %s\n", imageWithDigest)
		})

		Context("verify-enterprise-contract task", func() {
			var generator tekton.VerifyEnterpriseContract
			var rekorHost string
			var verifyECTaskBundle string
			publicSecretName := "cosign-public-key"

			BeforeAll(func() {
				// Copy the public key from openshift-pipelines/signing-secrets to a new
				// secret that contains just the public key to ensure that access
				// to password and private key are not needed.
				publicKey, err := fwk.AsKubeAdmin.TektonController.GetTektonChainsPublicKey()
				Expect(err).ToNot(HaveOccurred())
				GinkgoWriter.Printf("Copy public key from %s/signing-secrets to a new secret\n", constants.TEKTON_CHAINS_NS)
				Expect(fwk.AsKubeAdmin.TektonController.CreateOrUpdateSigningSecret(
					publicKey, publicSecretName, namespace)).To(Succeed())

				rekorHost, err = fwk.AsKubeAdmin.TektonController.GetRekorHost()
				Expect(err).ToNot(HaveOccurred())
				GinkgoWriter.Printf("Configured Rekor host: %s\n", rekorHost)

				cm, err := fwk.AsKubeAdmin.CommonController.GetConfigMap("ec-defaults", "enterprise-contract-service")
				Expect(err).ToNot(HaveOccurred())
				verifyECTaskBundle = cm.Data["verify_ec_task_bundle"]
				Expect(verifyECTaskBundle).ToNot(BeEmpty())
				GinkgoWriter.Printf("Using verify EC task bundle: %s\n", verifyECTaskBundle)
			})

			BeforeEach(func() {
				generator = tekton.VerifyEnterpriseContract{
					TaskBundle:          verifyECTaskBundle,
					Name:                "verify-enterprise-contract",
					Namespace:           namespace,
					PolicyConfiguration: "ec-policy",
					PublicKey:           fmt.Sprintf("k8s://%s/%s", namespace, publicSecretName),
					Strict:              true,
					EffectiveTime:       "now",
					IgnoreRekor:         true,
				}
				generator.WithComponentImage(imageWithDigest)

				// Since specs could update the config policy, make sure it has a consistent
				// baseline at the start of each spec.
				baselinePolicies := contract.PolicySpecWithSourceConfig(
					// A simple policy that should always succeed in a cluster where
					// Tekton Chains is properly setup.
					defaultECP.Spec, ecp.SourceConfig{Include: []string{"slsa_provenance_available"}})
				Expect(fwk.AsKubeAdmin.TektonController.CreateOrUpdatePolicyConfiguration(namespace, baselinePolicies)).To(Succeed())
				// printPolicyConfiguration(baselinePolicies)
			})

			It("succeeds when policy is met", func() {
				pr, err := fwk.AsKubeAdmin.TektonController.RunPipeline(generator, namespace, pipelineRunTimeout)
				Expect(err).NotTo(HaveOccurred())
				Expect(fwk.AsKubeAdmin.TektonController.WatchPipelineRun(pr.Name, namespace, pipelineRunTimeout)).To(Succeed())

				// Refresh our copy of the PipelineRun for latest results
				pr, err = fwk.AsKubeAdmin.TektonController.GetPipelineRun(pr.Name, pr.Namespace)
				Expect(err).NotTo(HaveOccurred())

				tr, err := fwk.AsKubeAdmin.TektonController.GetTaskRunStatus(fwk.AsKubeAdmin.CommonController.KubeRest(), pr, "verify-enterprise-contract")
				Expect(err).NotTo(HaveOccurred())
				printTaskRunStatus(tr, namespace, *kubeClient.CommonController)
				GinkgoWriter.Printf("Make sure TaskRun %s of PipelineRun %s succeeded\n", tr.PipelineTaskName, pr.Name)
				Expect(tekton.DidTaskRunSucceed(tr)).To(BeTrue())
				GinkgoWriter.Printf("Make sure result for TaskRun %q succeeded\n", tr.PipelineTaskName)
				Expect(tr.Status.TaskRunStatusFields.Results).Should(Or(
					ContainElements(tekton.MatchTaskRunResultWithJSONPathValue(constants.TektonTaskTestOutputName, "{$.result}", `["SUCCESS"]`)),
				))
			})

			It("does not pass when tests are not satisfied on non-strict mode", func() {
				policy := contract.PolicySpecWithSourceConfig(
					// The BuildahDemo pipeline used to generate the test data does not
					// include the required test tasks, so this policy should always fail.
					defaultECP.Spec, ecp.SourceConfig{Include: []string{"test"}})
				Expect(fwk.AsKubeAdmin.TektonController.CreateOrUpdatePolicyConfiguration(namespace, policy)).To(Succeed())
				// printPolicyConfiguration(policy)
				generator.Strict = false
				pr, err := fwk.AsKubeAdmin.TektonController.RunPipeline(generator, namespace, pipelineRunTimeout)
				Expect(err).NotTo(HaveOccurred())
				Expect(fwk.AsKubeAdmin.TektonController.WatchPipelineRun(pr.Name, namespace, pipelineRunTimeout)).To(Succeed())

				// Refresh our copy of the PipelineRun for latest results
				pr, err = fwk.AsKubeAdmin.TektonController.GetPipelineRun(pr.Name, pr.Namespace)
				Expect(err).NotTo(HaveOccurred())

				tr, err := fwk.AsKubeAdmin.TektonController.GetTaskRunStatus(fwk.AsKubeAdmin.CommonController.KubeRest(), pr, "verify-enterprise-contract")
				Expect(err).NotTo(HaveOccurred())

				printTaskRunStatus(tr, namespace, *kubeClient.CommonController)
				GinkgoWriter.Printf("Make sure TaskRun %s of PipelineRun %s succeeded\n", tr.PipelineTaskName, pr.Name)
				Expect(tekton.DidTaskRunSucceed(tr)).To(BeTrue())
				GinkgoWriter.Printf("Make sure result for TaskRun %q succeeded\n", tr.PipelineTaskName)
				Expect(tr.Status.TaskRunStatusFields.Results).Should(Or(
					ContainElements(tekton.MatchTaskRunResultWithJSONPathValue(constants.TektonTaskTestOutputName, "{$.result}", `["FAILURE"]`)),
				))
			})

			It("fails when tests are not satisfied on strict mode", func() {
				policy := contract.PolicySpecWithSourceConfig(
					// The BuildahDemo pipeline used to generate the test data does not
					// include the required test tasks, so this policy should always fail.
					defaultECP.Spec, ecp.SourceConfig{Include: []string{"test"}})
				Expect(fwk.AsKubeAdmin.TektonController.CreateOrUpdatePolicyConfiguration(namespace, policy)).To(Succeed())
				// printPolicyConfiguration(policy)

				generator.Strict = true
				pr, err := fwk.AsKubeAdmin.TektonController.RunPipeline(generator, namespace, pipelineRunTimeout)
				Expect(err).NotTo(HaveOccurred())
				Expect(fwk.AsKubeAdmin.TektonController.WatchPipelineRun(pr.Name, namespace, pipelineRunTimeout)).To(Succeed())

				// Refresh our copy of the PipelineRun for latest results
				pr, err = fwk.AsKubeAdmin.TektonController.GetPipelineRun(pr.Name, pr.Namespace)
				Expect(err).NotTo(HaveOccurred())

				tr, err := fwk.AsKubeAdmin.TektonController.GetTaskRunStatus(fwk.AsKubeAdmin.CommonController.KubeRest(), pr, "verify-enterprise-contract")
				Expect(err).NotTo(HaveOccurred())

				printTaskRunStatus(tr, namespace, *kubeClient.CommonController)
				GinkgoWriter.Printf("Make sure TaskRun %s of PipelineRun %s failed\n", tr.PipelineTaskName, pr.Name)
				Expect(tekton.DidTaskRunSucceed(tr)).To(BeFalse())
				// Because the task fails, no results are created
			})

			It("fails when unexpected signature is used", func() {
				secretName := fmt.Sprintf("dummy-public-key-%s", util.GenerateRandomString(10))
				publicKey := []byte("-----BEGIN PUBLIC KEY-----\n" +
					"MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAENZxkE/d0fKvJ51dXHQmxXaRMTtVz\n" +
					"BQWcmJD/7pcMDEmBcmk8O1yUPIiFj5TMZqabjS9CQQN+jKHG+Bfi0BYlHg==\n" +
					"-----END PUBLIC KEY-----")
				GinkgoWriter.Println("Create an unexpected public signing key")
				Expect(fwk.AsKubeAdmin.TektonController.CreateOrUpdateSigningSecret(publicKey, secretName, namespace)).To(Succeed())
				generator.PublicKey = fmt.Sprintf("k8s://%s/%s", namespace, secretName)

				pr, err := fwk.AsKubeAdmin.TektonController.RunPipeline(generator, namespace, pipelineRunTimeout)
				Expect(err).NotTo(HaveOccurred())
				Expect(fwk.AsKubeAdmin.TektonController.WatchPipelineRun(pr.Name, namespace, pipelineRunTimeout)).To(Succeed())

				// Refresh our copy of the PipelineRun for latest results
				pr, err = fwk.AsKubeAdmin.TektonController.GetPipelineRun(pr.Name, pr.Namespace)
				Expect(err).NotTo(HaveOccurred())

				tr, err := fwk.AsKubeAdmin.TektonController.GetTaskRunStatus(fwk.AsKubeAdmin.CommonController.KubeRest(), pr, "verify-enterprise-contract")
				Expect(err).NotTo(HaveOccurred())

				printTaskRunStatus(tr, namespace, *kubeClient.CommonController)
				GinkgoWriter.Printf("Make sure TaskRun %s of PipelineRun %s failed\n", tr.PipelineTaskName, pr.Name)
				Expect(tekton.DidTaskRunSucceed(tr)).To(BeFalse())
				// Because the task fails, no results are created
			})

			Context("ec-cli command", func() {
				It("verifies ec cli has error handling", func() {
					generator.WithComponentImage("quay.io/konflux-ci/ec-golden-image:latest")
					pr, err := fwk.AsKubeAdmin.TektonController.RunPipeline(generator, namespace, pipelineRunTimeout)
					Expect(err).NotTo(HaveOccurred())
					Expect(fwk.AsKubeAdmin.TektonController.WatchPipelineRun(pr.Name, namespace, pipelineRunTimeout)).To(Succeed())

					pr, err = fwk.AsKubeAdmin.TektonController.GetPipelineRun(pr.Name, pr.Namespace)
					Expect(err).NotTo(HaveOccurred())

					tr, err := fwk.AsKubeAdmin.TektonController.GetTaskRunStatus(fwk.AsKubeAdmin.CommonController.KubeRest(), pr, "verify-enterprise-contract")
					Expect(err).NotTo(HaveOccurred())

					Expect(tr.Status.TaskRunStatusFields.Results).Should(Or(
						ContainElements(tekton.MatchTaskRunResultWithJSONPathValue(constants.TektonTaskTestOutputName, "{$.result}", `["FAILURE"]`)),
					))
					//Get container step-report-json log details from pod
					reportLog, err := utils.GetContainerLogs(fwk.AsKubeAdmin.CommonController.KubeInterface(), tr.Status.PodName, "step-report-json", namespace)
					GinkgoWriter.Printf("*** Logs from pod '%s', container '%s':\n----- START -----%s----- END -----\n", tr.Status.PodName, "step-report-json", reportLog)
					Expect(err).NotTo(HaveOccurred())
					Expect(reportLog).Should(ContainSubstring("No image attestations found matching the given public key"))
				})

				It("verifies ec validate accepts a list of image references", func() {
					secretName := fmt.Sprintf("golden-image-public-key%s", util.GenerateRandomString(10))
					GinkgoWriter.Println("Update public key to verify golden images")
					goldenImagePublicKey := []byte("-----BEGIN PUBLIC KEY-----\n" +
						"MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEZP/0htjhVt2y0ohjgtIIgICOtQtA\n" +
						"naYJRuLprwIv6FDhZ5yFjYUEtsmoNcW7rx2KM6FOXGsCX3BNc7qhHELT+g==\n" +
						"-----END PUBLIC KEY-----")
					Expect(fwk.AsKubeAdmin.TektonController.CreateOrUpdateSigningSecret(goldenImagePublicKey, secretName, namespace)).To(Succeed())
					generator.PublicKey = fmt.Sprintf("k8s://%s/%s", namespace, secretName)

					policy := contract.PolicySpecWithSourceConfig(
						defaultECP.Spec,
						ecp.SourceConfig{
							Include: []string{"@slsa3"},
							// This test validates an image via a floating tag (as designed). This makes
							// it hard to provide the expected git commit. Here we just ignore that
							// particular check.
							Exclude: []string{"slsa_source_correlated.source_code_reference_provided"},
						})
					Expect(fwk.AsKubeAdmin.TektonController.CreateOrUpdatePolicyConfiguration(namespace, policy)).To(Succeed())

					generator.WithComponentImage("quay.io/konflux-ci/ec-golden-image:latest")
					generator.AppendComponentImage("quay.io/konflux-ci/ec-golden-image:e2e-test-unacceptable-task")
					pr, err := fwk.AsKubeAdmin.TektonController.RunPipeline(generator, namespace, pipelineRunTimeout)
					Expect(err).NotTo(HaveOccurred())
					Expect(fwk.AsKubeAdmin.TektonController.WatchPipelineRun(pr.Name, namespace, pipelineRunTimeout)).To(Succeed())

					pr, err = fwk.AsKubeAdmin.TektonController.GetPipelineRun(pr.Name, pr.Namespace)
					Expect(err).NotTo(HaveOccurred())

					tr, err := fwk.AsKubeAdmin.TektonController.GetTaskRunStatus(fwk.AsKubeAdmin.CommonController.KubeRest(), pr, "verify-enterprise-contract")
					Expect(err).NotTo(HaveOccurred())

					Expect(tr.Status.TaskRunStatusFields.Results).Should(Or(
						ContainElements(tekton.MatchTaskRunResultWithJSONPathValue(constants.TektonTaskTestOutputName, "{$.result}", `["SUCCESS"]`)),
					))
					//Get container step-report-json log details from pod
					reportLog, err := utils.GetContainerLogs(fwk.AsKubeAdmin.CommonController.KubeInterface(), tr.Status.PodName, "step-report-json", namespace)
					GinkgoWriter.Printf("*** Logs from pod '%s', container '%s':\n----- START -----%s----- END -----\n", tr.Status.PodName, "step-report-json", reportLog)
					Expect(err).NotTo(HaveOccurred())
				})
			})

			Context("Release Policy", func() {
				It("verifies redhat products pass the redhat policy rule collection before release ", func() {
					secretName := fmt.Sprintf("golden-image-public-key%s", util.GenerateRandomString(10))
					GinkgoWriter.Println("Update public key to verify golden images")
					goldenImagePublicKey := []byte("-----BEGIN PUBLIC KEY-----\n" +
						"MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEZP/0htjhVt2y0ohjgtIIgICOtQtA\n" +
						"naYJRuLprwIv6FDhZ5yFjYUEtsmoNcW7rx2KM6FOXGsCX3BNc7qhHELT+g==\n" +
						"-----END PUBLIC KEY-----")
					Expect(fwk.AsKubeAdmin.TektonController.CreateOrUpdateSigningSecret(goldenImagePublicKey, secretName, namespace)).To(Succeed())
					redhatECP, error := fwk.AsKubeAdmin.TektonController.GetEnterpriseContractPolicy("redhat", "enterprise-contract-service")
					Expect(error).NotTo(HaveOccurred())
					generator.PublicKey = fmt.Sprintf("k8s://%s/%s", namespace, secretName)
					policy := contract.PolicySpecWithSourceConfig(
						redhatECP.Spec,
						ecp.SourceConfig{
							Include: []string{"@redhat"},
							// This test validates an image via a floating tag (as designed). This makes
							// it hard to provide the expected git commit. Here we just ignore that
							// particular check.
							Exclude: []string{"slsa_source_correlated.source_code_reference_provided", "cve.cve_results_found"},
						})
					Expect(fwk.AsKubeAdmin.TektonController.CreateOrUpdatePolicyConfiguration(namespace, policy)).To(Succeed())

					generator.WithComponentImage("quay.io/konflux-ci/ec-golden-image:latest")
					pr, err := fwk.AsKubeAdmin.TektonController.RunPipeline(generator, namespace, pipelineRunTimeout)
					Expect(err).NotTo(HaveOccurred())
					Expect(fwk.AsKubeAdmin.TektonController.WatchPipelineRun(pr.Name, namespace, pipelineRunTimeout)).To(Succeed())

					pr, err = fwk.AsKubeAdmin.TektonController.GetPipelineRun(pr.Name, pr.Namespace)
					Expect(err).NotTo(HaveOccurred())

					tr, err := fwk.AsKubeAdmin.TektonController.GetTaskRunStatus(fwk.AsKubeAdmin.CommonController.KubeRest(), pr, "verify-enterprise-contract")
					Expect(err).NotTo(HaveOccurred())

					Expect(tr.Status.TaskRunStatusFields.Results).ShouldNot(Or(
						ContainElements(tekton.MatchTaskRunResultWithJSONPathValue(constants.TektonTaskTestOutputName, "{$.result}", `["FAILURE"]`)),
					))
				})
				It("verifies the release policy: Task are trusted", func() {
					secretName := fmt.Sprintf("golden-image-public-key%s", util.GenerateRandomString(10))
					GinkgoWriter.Println("Update public key to verify golden images")
					goldenImagePublicKey := []byte("-----BEGIN PUBLIC KEY-----\n" +
						"MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEZP/0htjhVt2y0ohjgtIIgICOtQtA\n" +
						"naYJRuLprwIv6FDhZ5yFjYUEtsmoNcW7rx2KM6FOXGsCX3BNc7qhHELT+g==\n" +
						"-----END PUBLIC KEY-----")
					Expect(fwk.AsKubeAdmin.TektonController.CreateOrUpdateSigningSecret(goldenImagePublicKey, secretName, namespace)).To(Succeed())
					generator.PublicKey = fmt.Sprintf("k8s://%s/%s", namespace, secretName)
					policy := contract.PolicySpecWithSourceConfig(
						defaultECP.Spec,
						ecp.SourceConfig{Include: []string{
							// Account for "acceptable" to "trusted" renaming. Eventually remove "acceptable" from this list
							"trusted_task.trusted",
						}},
					)
					Expect(fwk.AsKubeAdmin.TektonController.CreateOrUpdatePolicyConfiguration(namespace, policy)).To(Succeed())

					generator.WithComponentImage("quay.io/konflux-ci/ec-golden-image:e2e-test-unacceptable-task")
					pr, err := fwk.AsKubeAdmin.TektonController.RunPipeline(generator, namespace, pipelineRunTimeout)
					Expect(err).NotTo(HaveOccurred())
					Expect(fwk.AsKubeAdmin.TektonController.WatchPipelineRun(pr.Name, namespace, pipelineRunTimeout)).To(Succeed())

					pr, err = fwk.AsKubeAdmin.TektonController.GetPipelineRun(pr.Name, pr.Namespace)
					Expect(err).NotTo(HaveOccurred())

					tr, err := fwk.AsKubeAdmin.TektonController.GetTaskRunStatus(fwk.AsKubeAdmin.CommonController.KubeRest(), pr, "verify-enterprise-contract")
					Expect(err).NotTo(HaveOccurred())

					Expect(tr.Status.TaskRunStatusFields.Results).Should(Or(
						ContainElements(tekton.MatchTaskRunResultWithJSONPathValue(constants.TektonTaskTestOutputName, "{$.result}", `["FAILURE"]`)),
					))

					//Get container step-report-json log details from pod
					reportLog, err := utils.GetContainerLogs(fwk.AsKubeAdmin.CommonController.KubeInterface(), tr.Status.PodName, "step-report-json", namespace)
					GinkgoWriter.Printf("*** Logs from pod '%s', container '%s':\n----- START -----%s----- END -----\n", tr.Status.PodName, "step-report-json", reportLog)
					Expect(err).NotTo(HaveOccurred())
					Expect(reportLog).Should(MatchRegexp(`Pipeline task .* uses an untrusted task reference`))
				})

				It("verifies the release policy: Task references are pinned", func() {
					secretName := fmt.Sprintf("unpinned-task-bundle-public-key%s", util.GenerateRandomString(10))
					unpinnedTaskPublicKey := []byte("-----BEGIN PUBLIC KEY-----\n" +
						"MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEPfwkY/ru2JRd6FSqIp7lT3gzjaEC\n" +
						"EAg+paWtlme2KNcostCsmIbwz+bc2aFV+AxCOpRjRpp3vYrbS5KhkmgC1Q==\n" +
						"-----END PUBLIC KEY-----")
					GinkgoWriter.Println("Update public <key to verify unpinned task image")
					Expect(fwk.AsKubeAdmin.TektonController.CreateOrUpdateSigningSecret(unpinnedTaskPublicKey, secretName, namespace)).To(Succeed())
					generator.PublicKey = fmt.Sprintf("k8s://%s/%s", namespace, secretName)

					policy := contract.PolicySpecWithSourceConfig(
						defaultECP.Spec,
						ecp.SourceConfig{Include: []string{"trusted_task.pinned"}},
					)
					Expect(fwk.AsKubeAdmin.TektonController.CreateOrUpdatePolicyConfiguration(namespace, policy)).To(Succeed())

					generator.WithComponentImage("quay.io/redhat-appstudio-qe/enterprise-contract-tests:e2e-test-unpinned-task-bundle")
					pr, err := fwk.AsKubeAdmin.TektonController.RunPipeline(generator, namespace, pipelineRunTimeout)
					Expect(err).NotTo(HaveOccurred())
					Expect(fwk.AsKubeAdmin.TektonController.WatchPipelineRun(pr.Name, namespace, pipelineRunTimeout)).To(Succeed())

					pr, err = fwk.AsKubeAdmin.TektonController.GetPipelineRun(pr.Name, pr.Namespace)
					Expect(err).NotTo(HaveOccurred())

					tr, err := fwk.AsKubeAdmin.TektonController.GetTaskRunStatus(fwk.AsKubeAdmin.CommonController.KubeRest(), pr, "verify-enterprise-contract")
					Expect(err).NotTo(HaveOccurred())

					Expect(tr.Status.TaskRunStatusFields.Results).Should(Or(
						ContainElements(tekton.MatchTaskRunResultWithJSONPathValue(constants.TektonTaskTestOutputName, "{$.result}", `["WARNING"]`)),
					))

					//Get container step-report-json log details from pod
					reportLog, err := utils.GetContainerLogs(fwk.AsKubeAdmin.CommonController.KubeInterface(), tr.Status.PodName, "step-report-json", namespace)
					GinkgoWriter.Printf("*** Logs from pod '%s', container '%s':\n----- START -----%s----- END -----\n", tr.Status.PodName, "step-report-json", reportLog)
					Expect(err).NotTo(HaveOccurred())
					Expect(reportLog).Should(MatchRegexp(`Pipeline task .* uses an unpinned task reference`))
				})
			})
		})

	})
})

func printTaskRunStatus(tr *pipeline.PipelineRunTaskRunStatus, namespace string, sc common.SuiteController) {
	if tr.Status == nil {
		GinkgoWriter.Println("*** TaskRun status: nil")
		return
	}

	if y, err := yaml.Marshal(tr.Status); err == nil {
		GinkgoWriter.Printf("*** TaskRun status:\n%s\n", string(y))
	} else {
		GinkgoWriter.Printf("*** Unable to serialize TaskRunStatus to YAML: %#v; error: %s\n", tr.Status, err)
	}

	for _, s := range tr.Status.TaskRunStatusFields.Steps {
		if logs, err := utils.GetContainerLogs(sc.KubeInterface(), tr.Status.PodName, s.Container, namespace); err == nil {
			GinkgoWriter.Printf("*** Logs from pod '%s', container '%s':\n----- START -----%s----- END -----\n", tr.Status.PodName, s.Container, logs)
		} else {
			GinkgoWriter.Printf("*** Can't fetch logs from pod '%s', container '%s': %s\n", tr.Status.PodName, s.Container, err)
		}
	}
}
