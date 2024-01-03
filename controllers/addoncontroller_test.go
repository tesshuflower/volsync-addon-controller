package controllers_test

import (
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stolostron/volsync-addon-controller/controllers"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"open-cluster-management.io/addon-framework/pkg/addonfactory"
	addonv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	workv1 "open-cluster-management.io/api/work/v1"

	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"

	volsyncaddonv1alpha1 "github.com/stolostron/volsync-addon-controller/api/v1alpha1"
)

var _ = Describe("Addoncontroller", func() {
	logger := zap.New(zap.UseDevMode(true), zap.WriteTo(GinkgoWriter))

	genericCodecs := serializer.NewCodecFactory(scheme.Scheme)
	genericCodec := genericCodecs.UniversalDeserializer()

	// Make sure a ClusterManagementAddOn exists for volsync or addon-framework will not reconcile
	// VolSync ManagedClusterAddOns
	var clusterManagementAddon *addonv1alpha1.ClusterManagementAddOn

	BeforeEach(func() {
		// clustermanagementaddon (this is a global resource)
		clusterManagementAddon = &addonv1alpha1.ClusterManagementAddOn{
			ObjectMeta: metav1.ObjectMeta{
				Name: "volsync",
			},
			Spec: addonv1alpha1.ClusterManagementAddOnSpec{
				AddOnMeta: addonv1alpha1.AddOnMeta{
					DisplayName: "VolSync",
					Description: "VolSync",
				},
				SupportedConfigs: []addonv1alpha1.ConfigMeta{
					{
						ConfigGroupResource: addonv1alpha1.ConfigGroupResource{
							Group:    "addon.open-cluster-management.io",
							Resource: "addondeploymentconfigs",
						},
					},
					{
						ConfigGroupResource: addonv1alpha1.ConfigGroupResource{
							Group:    "addon.open-cluster-management.io",
							Resource: "volsyncaddonconfigs",
						},
					},
				},
				InstallStrategy: addonv1alpha1.InstallStrategy{
					Type: addonv1alpha1.AddonInstallStrategyManual,
				},
			},
		}
	})
	AfterEach(func() {
		Expect(testK8sClient.Delete(testCtx, clusterManagementAddon)).To(Succeed())
	})

	JustBeforeEach(func() {
		// Create the clustermanagementaddon here so tests can modify it in their BeforeEach()
		// before we create it
		Expect(testK8sClient.Create(testCtx, clusterManagementAddon)).To(Succeed())
	})

	Context("When a ManagedClusterExists", func() {
		var testManagedCluster *clusterv1.ManagedCluster
		var testManagedClusterNamespace *corev1.Namespace

		BeforeEach(func() {
			// Create a managed cluster CR to use for this test
			testManagedCluster = &clusterv1.ManagedCluster{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "addon-mgdcluster-",
					Labels: map[string]string{
						"vendor": "OpenShift",
					},
				},
			}

			Expect(testK8sClient.Create(testCtx, testManagedCluster)).To(Succeed())
			Expect(testManagedCluster.Name).NotTo(BeEmpty())

			// Fake the status of the mgd cluster to be available
			Eventually(func() error {
				err := testK8sClient.Get(testCtx, client.ObjectKeyFromObject(testManagedCluster), testManagedCluster)
				if err != nil {
					return err
				}

				clusterAvailableCondition := metav1.Condition{
					Type:    clusterv1.ManagedClusterConditionAvailable,
					Status:  metav1.ConditionTrue,
					Reason:  "testupdate",
					Message: "faking cluster available for test",
				}
				meta.SetStatusCondition(&testManagedCluster.Status.Conditions, clusterAvailableCondition)

				return testK8sClient.Status().Update(testCtx, testManagedCluster)
			}, timeout, interval).Should(Succeed())

			// Create a matching namespace for this managed cluster
			// (namespace with name=managedclustername is expected to exist on the hub)
			testManagedClusterNamespace = &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: testManagedCluster.GetName(),
				},
			}
			Expect(testK8sClient.Create(testCtx, testManagedClusterNamespace)).To(Succeed())
		})

		Context("When a ManagedClusterAddon for this addon is created", func() {
			var mcAddon *addonv1alpha1.ManagedClusterAddOn
			var manifestWork *workv1.ManifestWork
			BeforeEach(func() {
				// Create a ManagedClusterAddon for the mgd cluster
				mcAddon = &addonv1alpha1.ManagedClusterAddOn{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "volsync",
						Namespace: testManagedCluster.GetName(),
					},
					Spec: addonv1alpha1.ManagedClusterAddOnSpec{}, // Setting spec to empty
				}
			})
			JustBeforeEach(func() {
				// Create the managed cluster addon
				Expect(testK8sClient.Create(testCtx, mcAddon)).To(Succeed())

				manifestWork = &workv1.ManifestWork{}
				// The controller should create a ManifestWork for this ManagedClusterAddon
				Eventually(func() bool {
					allMwList := &workv1.ManifestWorkList{}
					Expect(testK8sClient.List(testCtx, allMwList,
						client.InNamespace(testManagedCluster.GetName()))).To(Succeed())

					for _, mw := range allMwList.Items {
						// addon-framework now creates manifestwork with "-0" prefix (to allow for
						// creating multiple manifestworks if the content is large - will not be the case
						// for VolSync - so we could alternatively just search for addon-volsync-deploy-0)
						if strings.HasPrefix(mw.GetName(), "addon-volsync-deploy") == true {
							manifestWork = &mw
							return true /* found the manifestwork */
						}
					}
					return false
				}, timeout, interval).Should(BeTrue())

				Expect(manifestWork).ToNot(BeNil())
			})

			Context("When installing into openshift-operators namespace (the default)", func() {
				var operatorSubscription *operatorsv1alpha1.Subscription

				JustBeforeEach(func() {
					// When installing into the global operator namespace (openshift-operators)
					// we should expect the manifestwork to contain only:
					// - the operator subscription
					Expect(len(manifestWork.Spec.Workload.Manifests)).To(Equal(1))

					// Subscription
					subMF := manifestWork.Spec.Workload.Manifests[0]
					subObj, _, err := genericCodec.Decode(subMF.Raw, nil, nil)
					Expect(err).NotTo(HaveOccurred())
					var ok bool
					operatorSubscription, ok = subObj.(*operatorsv1alpha1.Subscription)
					Expect(ok).To(BeTrue())
					Expect(operatorSubscription).NotTo(BeNil())
					Expect(operatorSubscription.GetNamespace()).To(Equal("openshift-operators"))
					Expect(operatorSubscription.Spec.Package).To(Equal("volsync-product")) // This is the "name" in json

					// No addonDeploymentConfig in these tests, so the operatorSub should not have any Config specified
					Expect(operatorSubscription.Spec.Config).To(BeNil())

					// More specific checks done in tests
				})

				Context("When the ManagedClusterAddOn spec set an installNamespace", func() {
					BeforeEach(func() {
						// Override to specifically set the ns in the spec - all the tests above in JustBeforeEach
						// should still be valid here
						mcAddon.Spec.InstallNamespace = "test1234"
					})
					It("Should still install to the default openshift-operators namespace", func() {
						// Code shouldn't have alterted the spec - but tests above will confirm that the
						// operatorgroup/subscription were created in volsync-system
						Expect(mcAddon.Spec.InstallNamespace).To(Equal("test1234"))
					})
				})

				Context("When no annotations are on the managedclusteraddon", func() {
					It("Should create the subscription (within the ManifestWork) with proper defaults", func() {
						// This ns is now the default in the mcao crd so will be used - note we ignore this
						// and use openshift-operators (see the created subscription)
						Expect(mcAddon.Spec.InstallNamespace).To(Equal("open-cluster-management-agent-addon"))

						Expect(operatorSubscription.Spec.Channel).To(Equal(controllers.DefaultChannel))
						Expect(string(operatorSubscription.Spec.InstallPlanApproval)).To(Equal(
							controllers.DefaultInstallPlanApproval))
						Expect(operatorSubscription.Spec.CatalogSource).To(Equal(controllers.DefaultCatalogSource))
						Expect(operatorSubscription.Spec.CatalogSourceNamespace).To(Equal(
							controllers.DefaultCatalogSourceNamespace))
						Expect(operatorSubscription.Spec.StartingCSV).To(Equal(controllers.DefaultStartingCSV))
					})
				})

				Context("When the annotation to override the CatalogSource is on the managedclusteraddon", func() {
					BeforeEach(func() {
						mcAddon.Annotations = map[string]string{
							controllers.AnnotationCatalogSourceOverride: "customcatalog-source",
						}
					})
					It("Should create the subscription (within the ManifestWork) with proper CatalogSource", func() {
						Expect(operatorSubscription.Spec.CatalogSource).To(Equal("customcatalog-source"))

						// The rest should be defaults
						Expect(operatorSubscription.Spec.Channel).To(Equal(controllers.DefaultChannel))
						Expect(string(operatorSubscription.Spec.InstallPlanApproval)).To(Equal(
							controllers.DefaultInstallPlanApproval))
						Expect(operatorSubscription.Spec.CatalogSourceNamespace).To(Equal(
							controllers.DefaultCatalogSourceNamespace))
						Expect(operatorSubscription.Spec.StartingCSV).To(Equal(controllers.DefaultStartingCSV))

					})
				})

				Context("When the annotation to override the CatalogSourceNS is on the managedclusteraddon", func() {
					BeforeEach(func() {
						mcAddon.Annotations = map[string]string{
							controllers.AnnotationCatalogSourceNamespaceOverride: "my-catalog-source-ns",
						}
					})
					It("Should create the subscription (within the ManifestWork) with proper CatalogSourceNS", func() {
						Expect(operatorSubscription.Spec.CatalogSourceNamespace).To(Equal(
							"my-catalog-source-ns"))

						// The rest should be defaults
						Expect(operatorSubscription.Spec.Channel).To(Equal(controllers.DefaultChannel))
						Expect(string(operatorSubscription.Spec.InstallPlanApproval)).To(Equal(
							controllers.DefaultInstallPlanApproval))
						Expect(operatorSubscription.Spec.CatalogSource).To(Equal(controllers.DefaultCatalogSource))
						Expect(operatorSubscription.Spec.StartingCSV).To(Equal(controllers.DefaultStartingCSV))

					})
				})

				Context("When the annotation to override the InstallPlanApproval is on the managedclusteraddon", func() {
					BeforeEach(func() {
						mcAddon.Annotations = map[string]string{
							controllers.AnnotationInstallPlanApprovalOverride: "Manual",
						}
					})
					It("Should create the subscription (within the ManifestWork) with proper CatalogSourceNS", func() {
						Expect(string(operatorSubscription.Spec.InstallPlanApproval)).To(Equal("Manual"))

						// The rest should be defaults
						Expect(operatorSubscription.Spec.Channel).To(Equal(controllers.DefaultChannel))
						Expect(operatorSubscription.Spec.CatalogSource).To(Equal(controllers.DefaultCatalogSource))
						Expect(operatorSubscription.Spec.CatalogSourceNamespace).To(Equal(
							controllers.DefaultCatalogSourceNamespace))
						Expect(operatorSubscription.Spec.StartingCSV).To(Equal(controllers.DefaultStartingCSV))

					})
				})

				Context("When the annotation to override the Channel is on the managedclusteraddon", func() {
					BeforeEach(func() {
						mcAddon.Annotations = map[string]string{
							controllers.AnnotationChannelOverride: "special-channel-1.2.3",
						}
					})
					It("Should create the subscription (within the ManifestWork) with proper CatalogSourceNS", func() {
						Expect(operatorSubscription.Spec.Channel).To(Equal("special-channel-1.2.3"))

						// The rest should be defaults
						Expect(string(operatorSubscription.Spec.InstallPlanApproval)).To(Equal(
							controllers.DefaultInstallPlanApproval))
						Expect(operatorSubscription.Spec.CatalogSource).To(Equal(controllers.DefaultCatalogSource))
						Expect(operatorSubscription.Spec.CatalogSourceNamespace).To(Equal(
							controllers.DefaultCatalogSourceNamespace))
						Expect(operatorSubscription.Spec.StartingCSV).To(Equal(controllers.DefaultStartingCSV))

					})
				})

				Context("When the annotation to override the StartingCSV is on the managedclusteraddon", func() {
					BeforeEach(func() {
						mcAddon.Annotations = map[string]string{
							controllers.AnnotationStartingCSVOverride: "volsync.v1.2.3.doesnotexist",
						}
					})
					It("Should create the subscription (within the ManifestWork) with proper CatalogSourceNS", func() {
						Expect(operatorSubscription.Spec.StartingCSV).To(Equal("volsync.v1.2.3.doesnotexist"))

						// The rest should be defaults
						Expect(operatorSubscription.Spec.Channel).To(Equal(controllers.DefaultChannel))
						Expect(string(operatorSubscription.Spec.InstallPlanApproval)).To(Equal(
							controllers.DefaultInstallPlanApproval))
						Expect(operatorSubscription.Spec.CatalogSource).To(Equal(controllers.DefaultCatalogSource))
						Expect(operatorSubscription.Spec.CatalogSourceNamespace).To(Equal(
							controllers.DefaultCatalogSourceNamespace))
					})
				})
			})
		})

		Context("When the manifestwork already exists", func() {
			// Make sure the addon-framework will tolerate upgrades where the managedclusteraddon previously
			// created the manifestwork with the name "addon-volsync-deploy".  Newer versions of the addon-framework
			// name the manifestwork "addon-volsync-deploy-0".  These tests ensure a migration from older behavior
			// to the new work ok.
			var fakeOlderMw *workv1.ManifestWork
			BeforeEach(func() {
				// First pre-create the manifestwork with the old name "addon-volsync-deploy" and to make it look
				// like it was deployed from an older version of volsync-addon-controller using the older
				// addon-framework.
				fakeOlderMw = &workv1.ManifestWork{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "addon-volsync-deploy", // old name generated by addon-framework < 0.5.0
						Namespace: testManagedCluster.GetName(),
						Labels: map[string]string{
							"open-cluster-management.io/addon-name": "volsync",
						},
					},
					Spec: workv1.ManifestWorkSpec{},
				}
				Expect(testK8sClient.Create(testCtx, fakeOlderMw)).To(Succeed())

				// Make sure cache loads this manifestwork before proceeding
				Eventually(func() error {
					return testK8sClient.Get(testCtx, client.ObjectKeyFromObject(fakeOlderMw), fakeOlderMw)
				}, timeout, interval).Should(Succeed())
			})

			It("Should use the existing manifestwork", func() {
				// Create a ManagedClusterAddon for the mgd cluster
				mcAddon := &addonv1alpha1.ManagedClusterAddOn{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "volsync",
						Namespace: testManagedCluster.GetName(),
					},
					Spec: addonv1alpha1.ManagedClusterAddOnSpec{}, // Setting spec to empty
				}

				Expect(testK8sClient.Create(testCtx, mcAddon)).To(Succeed())

				Eventually(func() bool {
					// List manifestworks - pre-existing manifestwork should still be there and be updated
					allMwList := &workv1.ManifestWorkList{}
					Expect(testK8sClient.List(testCtx, allMwList,
						client.InNamespace(testManagedCluster.GetName()))).To(Succeed())

					if len(allMwList.Items) != 1 {
						// it's either created another manifestwork (bad) or deleted the existing one (also bad)
						return false
					}

					myMw := &allMwList.Items[0]
					Expect(myMw.GetName()).To(Equal(fakeOlderMw.GetName()))

					if len(myMw.Spec.Workload.Manifests) != 1 {
						// Manifestwork hasn't been updated with the subscription yet
						return false
					}

					Expect(myMw.Spec.Workload.Manifests[0])
					subObj, _, err := genericCodec.Decode(myMw.Spec.Workload.Manifests[0].Raw, nil, nil)
					Expect(err).NotTo(HaveOccurred())
					operatorSubscription, ok := subObj.(*operatorsv1alpha1.Subscription)

					return ok && operatorSubscription != nil
				}, timeout, interval).Should(BeTrue())
			})
		})

		Describe("Node Selector/Tolerations tests", func() {
			Context("When a ManagedClusterAddOn is created", func() {
				var mcAddon *addonv1alpha1.ManagedClusterAddOn
				var manifestWork *workv1.ManifestWork
				var operatorSubscription *operatorsv1alpha1.Subscription

				BeforeEach(func() {
					// Create a ManagedClusterAddon for the mgd cluster using an addonDeploymentconfig
					mcAddon = &addonv1alpha1.ManagedClusterAddOn{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "volsync",
							Namespace: testManagedCluster.GetName(),
						},
						Spec: addonv1alpha1.ManagedClusterAddOnSpec{}, // Setting spec to empty
					}
				})
				JustBeforeEach(func() {
					// Create the managed cluster addon
					Expect(testK8sClient.Create(testCtx, mcAddon)).To(Succeed())

					manifestWork = &workv1.ManifestWork{}
					// The controller should create a ManifestWork for this ManagedClusterAddon
					Eventually(func() bool {
						allMwList := &workv1.ManifestWorkList{}
						Expect(testK8sClient.List(testCtx, allMwList,
							client.InNamespace(testManagedCluster.GetName()))).To(Succeed())

						for _, mw := range allMwList.Items {
							// addon-framework now creates manifestwork with "-0" prefix (to allow for
							// creating multiple manifestworks if the content is large - will not be the case
							// for VolSync - so we could alternatively just search for addon-volsync-deploy-0)
							if strings.HasPrefix(mw.GetName(), "addon-volsync-deploy") == true {
								manifestWork = &mw
								return true /* found the manifestwork */
							}
						}
						return false
					}, timeout, interval).Should(BeTrue())

					Expect(manifestWork).ToNot(BeNil())

					// Subscription
					subMF := manifestWork.Spec.Workload.Manifests[0]
					subObj, _, err := genericCodec.Decode(subMF.Raw, nil, nil)
					Expect(err).NotTo(HaveOccurred())
					var ok bool
					operatorSubscription, ok = subObj.(*operatorsv1alpha1.Subscription)
					Expect(ok).To(BeTrue())
					Expect(operatorSubscription).NotTo(BeNil())
					Expect(operatorSubscription.GetNamespace()).To(Equal("openshift-operators"))
					Expect(operatorSubscription.Spec.Package).To(Equal("volsync-product")) // This is the "name" in json
				})

				Context("When no addonDeploymentConfig is referenced", func() {
					It("Should create the sub in the manifestwork with no tolerations or selectors", func() {
						Expect(operatorSubscription.Spec.Config).To(BeNil())
					})

					Context("When the managedclusteraddon is updated later with a addondeploymentconfig", func() {
						var addonDeploymentConfig *addonv1alpha1.AddOnDeploymentConfig
						nodePlacement := &addonv1alpha1.NodePlacement{
							NodeSelector: map[string]string{
								"place": "here",
							},
							Tolerations: []corev1.Toleration{
								{
									Key:      "node.kubernetes.io/unreachable",
									Operator: corev1.TolerationOpExists,
									Effect:   corev1.TaintEffectNoSchedule,
								},
							},
						}

						BeforeEach(func() {
							addonDeploymentConfig = createAddonDeploymentConfig(nodePlacement)
						})
						AfterEach(func() {
							cleanupAddonDeploymentConfig(addonDeploymentConfig, true)
						})

						It("Should update the existing manifestwork with the addondeploymentconfig", func() {
							Expect(operatorSubscription.Spec.Config).To(BeNil())

							// Update the managedclusteraddon to reference the addonDeploymentConfig
							Eventually(func() error {
								err := testK8sClient.Get(testCtx, client.ObjectKeyFromObject(mcAddon), mcAddon)
								if err != nil {
									return err
								}

								// Update the managedclusteraddon - doing this in eventually loop to avoid update issues if
								// the controller is also updating the resource
								mcAddon.Spec.Configs = []addonv1alpha1.AddOnConfig{
									{
										ConfigGroupResource: addonv1alpha1.ConfigGroupResource{
											Group:    "addon.open-cluster-management.io",
											Resource: "addondeploymentconfigs",
										},
										ConfigReferent: addonv1alpha1.ConfigReferent{
											Name:      addonDeploymentConfig.GetName(),
											Namespace: addonDeploymentConfig.GetNamespace(),
										},
									},
								}

								return testK8sClient.Update(testCtx, mcAddon)
							}, timeout, interval).Should(Succeed())

							// Now reload the manifestwork, it should eventually be updated with the nodeselector
							// and tolerations

							var manifestWorkReloaded *workv1.ManifestWork
							var operatorSubscriptionReloaded *operatorsv1alpha1.Subscription

							Eventually(func() bool {
								allMwList := &workv1.ManifestWorkList{}
								Expect(testK8sClient.List(testCtx, allMwList,
									client.InNamespace(testManagedCluster.GetName()))).To(Succeed())

								for _, mw := range allMwList.Items {
									// addon-framework now creates manifestwork with "-0" prefix (to allow for
									// creating multiple manifestworks if the content is large - will not be the case
									// for VolSync - so we could alternatively just search for addon-volsync-deploy-0)
									if strings.HasPrefix(mw.GetName(), "addon-volsync-deploy") == true {
										manifestWorkReloaded = &mw
										break
									}
								}

								if manifestWorkReloaded == nil {
									return false
								}

								// Subscription
								subMF := manifestWorkReloaded.Spec.Workload.Manifests[0]
								subObj, _, err := genericCodec.Decode(subMF.Raw, nil, nil)
								Expect(err).NotTo(HaveOccurred())
								var ok bool
								operatorSubscriptionReloaded, ok = subObj.(*operatorsv1alpha1.Subscription)
								Expect(ok).To(BeTrue())
								Expect(operatorSubscriptionReloaded).NotTo(BeNil())

								// If spec.config has been set, then it's been updated
								return operatorSubscriptionReloaded.Spec.Config != nil
							}, timeout, interval).Should(BeTrue())

							Expect(operatorSubscriptionReloaded.Spec.Config.NodeSelector).To(Equal(nodePlacement.NodeSelector))
							Expect(operatorSubscriptionReloaded.Spec.Config.Tolerations).To(Equal(nodePlacement.Tolerations))
						})
					})
				})

				Context("When the addonDeploymentconfig has nodeSelector and no tolerations", func() {
					var addonDeploymentConfig *addonv1alpha1.AddOnDeploymentConfig
					nodePlacement := &addonv1alpha1.NodePlacement{
						NodeSelector: map[string]string{
							"a":    "b",
							"1234": "5678",
						},
					}
					BeforeEach(func() {
						addonDeploymentConfig = createAddonDeploymentConfig(nodePlacement)

						// Update the managedclusteraddon before we create it to add the addondeploymentconfig
						mcAddon.Spec.Configs = []addonv1alpha1.AddOnConfig{
							{
								ConfigGroupResource: addonv1alpha1.ConfigGroupResource{
									Group:    "addon.open-cluster-management.io",
									Resource: "addondeploymentconfigs",
								},
								ConfigReferent: addonv1alpha1.ConfigReferent{
									Name:      addonDeploymentConfig.GetName(),
									Namespace: addonDeploymentConfig.GetNamespace(),
								},
							},
						}
					})
					AfterEach(func() {
						cleanupAddonDeploymentConfig(addonDeploymentConfig, true)
					})

					It("Should create the sub in the manifestwork wiith the node selector", func() {
						Expect(operatorSubscription.Spec.Config).ToNot(BeNil())
						Expect(operatorSubscription.Spec.Config.NodeSelector).To(Equal(nodePlacement.NodeSelector))
						Expect(operatorSubscription.Spec.Config.Tolerations).To(BeNil()) // No tolerations set
					})
				})

				Context("When the addonDeployment config has tolerations and no nodeSelector", func() {
					var addonDeploymentConfig *addonv1alpha1.AddOnDeploymentConfig
					nodePlacement := &addonv1alpha1.NodePlacement{
						Tolerations: []corev1.Toleration{
							{
								Key:      "node.kubernetes.io/unreachable",
								Operator: corev1.TolerationOpExists,
								Effect:   corev1.TaintEffectNoSchedule,
							},
						},
					}
					BeforeEach(func() {
						addonDeploymentConfig = createAddonDeploymentConfig(nodePlacement)

						// Update the managedclusteraddon before we create it to add the addondeploymentconfig
						mcAddon.Spec.Configs = []addonv1alpha1.AddOnConfig{
							{
								ConfigGroupResource: addonv1alpha1.ConfigGroupResource{
									Group:    "addon.open-cluster-management.io",
									Resource: "addondeploymentconfigs",
								},
								ConfigReferent: addonv1alpha1.ConfigReferent{
									Name:      addonDeploymentConfig.GetName(),
									Namespace: addonDeploymentConfig.GetNamespace(),
								},
							},
						}
					})
					AfterEach(func() {
						cleanupAddonDeploymentConfig(addonDeploymentConfig, true)
					})

					It("Should create the sub in the manifestwork wiith the node selector", func() {
						Expect(operatorSubscription.Spec.Config).ToNot(BeNil())
						Expect(operatorSubscription.Spec.Config.Tolerations).To(Equal(nodePlacement.Tolerations))
						Expect(operatorSubscription.Spec.Config.NodeSelector).To(BeNil()) // No selectors set
					})
				})

				Context("When the addonDeployment config has nodeSelector and tolerations and nodeSelector", func() {
					var addonDeploymentConfig *addonv1alpha1.AddOnDeploymentConfig
					nodePlacement := &addonv1alpha1.NodePlacement{
						NodeSelector: map[string]string{
							"apples": "oranges",
						},
						Tolerations: []corev1.Toleration{
							{
								Key:      "node.kubernetes.io/unreachable",
								Operator: corev1.TolerationOpExists,
								Effect:   corev1.TaintEffectNoSchedule,
							},
							{
								Key:      "fakekey",
								Value:    "somevalue",
								Operator: corev1.TolerationOpEqual,
								Effect:   corev1.TaintEffectNoExecute,
							},
						},
					}
					BeforeEach(func() {
						addonDeploymentConfig = createAddonDeploymentConfig(nodePlacement)

						// Update the managedclusteraddon before we create it to add the addondeploymentconfig
						mcAddon.Spec.Configs = []addonv1alpha1.AddOnConfig{
							{
								ConfigGroupResource: addonv1alpha1.ConfigGroupResource{
									Group:    "addon.open-cluster-management.io",
									Resource: "addondeploymentconfigs",
								},
								ConfigReferent: addonv1alpha1.ConfigReferent{
									Name:      addonDeploymentConfig.GetName(),
									Namespace: addonDeploymentConfig.GetNamespace(),
								},
							},
						}
					})
					AfterEach(func() {
						cleanupAddonDeploymentConfig(addonDeploymentConfig, true)
					})

					It("Should create the sub in the manifestwork wiith the node selector", func() {
						Expect(operatorSubscription.Spec.Config).ToNot(BeNil())
						Expect(operatorSubscription.Spec.Config.NodeSelector).To(Equal(nodePlacement.NodeSelector))
						Expect(operatorSubscription.Spec.Config.Tolerations).To(Equal(nodePlacement.Tolerations))
					})

					Context("When a VolSyncAddOnConfig is also used with a cpuLimit", func() {
						cpuLimit, err := resource.ParseQuantity("500m")
						Expect(err).NotTo(HaveOccurred())

						volSyncAddOnConfig := &volsyncaddonv1alpha1.VolSyncAddOnConfig{
							ObjectMeta: metav1.ObjectMeta{
								Name: "test-vsaddonconfig",
							},
							Spec: &volsyncaddonv1alpha1.VolSyncAddOnConfigSpec{
								SubscriptionConfig: &operatorsv1alpha1.SubscriptionConfig{
									Resources: &corev1.ResourceRequirements{
										Limits: corev1.ResourceList{
											corev1.ResourceCPU: cpuLimit,
										},
									},
								},
							},
						}

						BeforeEach(func() {
							// Put this in the same ns as the addondeploymentconfig, it'll get cleaned up too when we delete the ns at end of test
							volSyncAddOnConfig.Namespace = addonDeploymentConfig.GetNamespace()
							Expect(testK8sClient.Create(testCtx, volSyncAddOnConfig)).To(Succeed())

							// Update the managedclusteraddon before we create it to add the volsyncaddonconfig
							mcAddon.Spec.Configs = append(mcAddon.Spec.Configs, addonv1alpha1.AddOnConfig{
								ConfigGroupResource: addonv1alpha1.ConfigGroupResource{
									Group:    "addon.open-cluster-management.io",
									Resource: "volsyncaddonconfigs",
								},
								ConfigReferent: addonv1alpha1.ConfigReferent{
									Name:      volSyncAddOnConfig.GetName(),
									Namespace: volSyncAddOnConfig.GetNamespace(),
								},
							})
						})

						It("Should use the cpuLimit from the volsyncaddonconfig", func() {
							Expect(operatorSubscription.Spec.Config).NotTo(BeNil())
							Expect(operatorSubscription.Spec.Config.NodeSelector).To(Equal(nodePlacement.NodeSelector)) // From addonDeloymentConfig
							Expect(operatorSubscription.Spec.Config.Tolerations).To(Equal(nodePlacement.Tolerations))   // From addonDeloymentConfig

							// Check cpu limit was set properly
							Expect(operatorSubscription.Spec.Config.Resources).NotTo(BeNil())
							Expect(operatorSubscription.Spec.Config.Resources.Limits).NotTo(BeNil())
							Expect(operatorSubscription.Spec.Config.Resources.Limits.Cpu().Cmp(cpuLimit)).To(Equal(0))
						})
					})

					Context("When a VolSyncAddOnConfig is also used with requests, nodeSelector, CatalogSource", func() {
						cpuLimit, err := resource.ParseQuantity("0.1")
						Expect(err).NotTo(HaveOccurred())
						cpuRequest, err := resource.ParseQuantity("50m")
						Expect(err).NotTo(HaveOccurred())
						memRequest, err := resource.ParseQuantity("20Mi")
						Expect(err).NotTo(HaveOccurred())

						volSyncAddOnConfig := &volsyncaddonv1alpha1.VolSyncAddOnConfig{
							ObjectMeta: metav1.ObjectMeta{
								Name: "test-lots-vsaddonconfig",
							},
							Spec: &volsyncaddonv1alpha1.VolSyncAddOnConfigSpec{
								SubscriptionCatalogSource: "my-test-catalog",
								SubscriptionConfig: &operatorsv1alpha1.SubscriptionConfig{
									NodeSelector: map[string]string{
										"newselectorkey": "selectorvalue",
										"bananas":        "grapefruits",
									},

									Resources: &corev1.ResourceRequirements{
										Limits: corev1.ResourceList{
											corev1.ResourceCPU: cpuLimit,
										},
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    cpuRequest,
											corev1.ResourceMemory: memRequest,
										},
									},
								},
							},
						}

						BeforeEach(func() {
							// Also set some annotations on the ManagedClusterAddOn
							mcAddon.Annotations = map[string]string{
								"operator-subscription-channel":         "testing-0.0",
								"operator-subscription-source":          "testing-catalog", // Should get overridden by vsAddonConfig
								"operator-subscription-sourceNamespace": "testing-marketplace-ns",
							}

							// Create VolSyncAddOnConfig
							// Put this in the same ns as the addondeploymentconfig, it'll get cleaned up too when we delete the ns at end of test
							volSyncAddOnConfig.Namespace = addonDeploymentConfig.GetNamespace()
							Expect(testK8sClient.Create(testCtx, volSyncAddOnConfig)).To(Succeed())

							// Update the managedclusteraddon before we create it to add the volsyncaddonconfig
							mcAddon.Spec.Configs = append(mcAddon.Spec.Configs, addonv1alpha1.AddOnConfig{
								ConfigGroupResource: addonv1alpha1.ConfigGroupResource{
									Group:    "addon.open-cluster-management.io",
									Resource: "volsyncaddonconfigs",
								},
								ConfigReferent: addonv1alpha1.ConfigReferent{
									Name:      volSyncAddOnConfig.GetName(),
									Namespace: volSyncAddOnConfig.GetNamespace(),
								},
							})
						})

						It("Should use the values from the volsyncaddonconfig", func() {
							logger.Info("Subscription", "operatorSubscription", operatorSubscription)
							Expect(operatorSubscription.Spec.Config).NotTo(BeNil())

							// Tolerations should still be from the addOnDeploymentConfig
							Expect(operatorSubscription.Spec.Config.Tolerations).To(Equal(nodePlacement.Tolerations)) // From addonDeloymentConfig

							// NodeSelector should use the value from the addOnDeploymentConfig not the VolSyncAddonConfig
							Expect(operatorSubscription.Spec.Config.NodeSelector).To(Equal(
								nodePlacement.NodeSelector)) // From addonDeloymentConfig

							// Check cpu limit was set properly
							Expect(operatorSubscription.Spec.Config.Resources).NotTo(BeNil())
							Expect(operatorSubscription.Spec.Config.Resources.Limits).NotTo(BeNil())
							Expect(operatorSubscription.Spec.Config.Resources.Limits.Cpu().Cmp(cpuLimit)).To(Equal(0))

							// Check cpu and mem requests were set properly
							Expect(operatorSubscription.Spec.Config.Resources.Requests).NotTo(BeNil())
							Expect(operatorSubscription.Spec.Config.Resources.Requests.Cpu().Cmp(cpuRequest)).To(Equal(0))
							Expect(operatorSubscription.Spec.Config.Resources.Requests.Memory().Cmp(memRequest)).To(Equal(0))

							// Check catalog source is correct (from volsyncaddonconfig)
							Expect(operatorSubscription.Spec.CatalogSource).To(Equal(
								volSyncAddOnConfig.Spec.SubscriptionCatalogSource))

							// Check that annotations on the mcao are used (the ones not overridden by volsyncaddonconfig)
							Expect(operatorSubscription.Spec.Channel).To(Equal("testing-0.0"))
							Expect(operatorSubscription.Spec.CatalogSourceNamespace).To(Equal("testing-marketplace-ns"))

							// Confirm other spec is still default
							Expect(string(operatorSubscription.Spec.InstallPlanApproval)).To(Equal(
								controllers.DefaultInstallPlanApproval))
							Expect(operatorSubscription.Spec.StartingCSV).To(Equal(controllers.DefaultStartingCSV))
						})
					})

					Context("When a volsyncaddonconfig is used and then later modified", func() {
						origCpuLimit, err := resource.ParseQuantity("0.2")
						Expect(err).NotTo(HaveOccurred())
						origMemLimit, err := resource.ParseQuantity("256Mi")
						Expect(err).NotTo(HaveOccurred())

						volSyncAddOnConfig := &volsyncaddonv1alpha1.VolSyncAddOnConfig{
							ObjectMeta: metav1.ObjectMeta{
								Name: "test-update-vsaddonconfig",
							},
							Spec: &volsyncaddonv1alpha1.VolSyncAddOnConfigSpec{
								SubscriptionCatalogSource: "my-test-catalog",
								SubscriptionConfig: &operatorsv1alpha1.SubscriptionConfig{
									Resources: &corev1.ResourceRequirements{
										Limits: corev1.ResourceList{
											corev1.ResourceCPU:    origCpuLimit,
											corev1.ResourceMemory: origMemLimit,
										},
									},
								},
							},
						}

						BeforeEach(func() {
							// Create VolSyncAddOnConfig
							// Put this in the same ns as the addondeploymentconfig, it'll get cleaned up too when
							// we delete the ns at end of test
							volSyncAddOnConfig.Namespace = addonDeploymentConfig.GetNamespace()
							Expect(testK8sClient.Create(testCtx, volSyncAddOnConfig)).To(Succeed())

							// Update the managedclusteraddon before we create it to add the volsyncaddonconfig
							mcAddon.Spec.Configs = append(mcAddon.Spec.Configs, addonv1alpha1.AddOnConfig{
								ConfigGroupResource: addonv1alpha1.ConfigGroupResource{
									Group:    "addon.open-cluster-management.io",
									Resource: "volsyncaddonconfigs",
								},
								ConfigReferent: addonv1alpha1.ConfigReferent{
									Name:      volSyncAddOnConfig.GetName(),
									Namespace: volSyncAddOnConfig.GetNamespace(),
								},
							})
						})

						It("Should reconcile the managedclusteraddon automatically and update", func() {
							logger.Info("Subscription", "operatorSubscription", operatorSubscription)
							Expect(operatorSubscription.Spec.Config).NotTo(BeNil())

							// Check limits were set properly (original values from volsyncaddonconfig)
							Expect(operatorSubscription.Spec.Config.Resources).NotTo(BeNil())
							Expect(operatorSubscription.Spec.Config.Resources.Limits).NotTo(BeNil())
							Expect(operatorSubscription.Spec.Config.Resources.Limits.Cpu().Cmp(origCpuLimit)).To(Equal(0))
							Expect(operatorSubscription.Spec.Config.Resources.Limits.Memory().Cmp(origMemLimit)).To(Equal(0))

							updatedCpuLimit, err := resource.ParseQuantity("800m")
							Expect(err).NotTo(HaveOccurred())
							updatedMemLimit, err := resource.ParseQuantity("2Gi")
							Expect(err).NotTo(HaveOccurred())

							// Now modify the volsyncaddonconfig and verify the changes are reconciled
							Eventually(func() error {
								// Update the volsyncaddonconfig
								err := testK8sClient.Get(testCtx, client.ObjectKeyFromObject(volSyncAddOnConfig), volSyncAddOnConfig)
								if err != nil {
									return err
								}
								volSyncAddOnConfig.Spec.SubscriptionConfig.Resources.Limits = corev1.ResourceList{
									corev1.ResourceCPU:    updatedCpuLimit,
									corev1.ResourceMemory: updatedMemLimit,
								}
								return testK8sClient.Update(testCtx, volSyncAddOnConfig)
							}, timeout, interval).Should(Succeed())

							// Now the controller should be alerted to do a reconcile and apply the updates
							Eventually(func() error {
								// Re-load the manifestwork
								err := testK8sClient.Get(testCtx, client.ObjectKeyFromObject(manifestWork), manifestWork)
								if err != nil {
									return err
								}

								// Extract the Subscription from the manifest work so we can check it
								subMF := manifestWork.Spec.Workload.Manifests[0]
								subObj, _, err := genericCodec.Decode(subMF.Raw, nil, nil)
								Expect(err).NotTo(HaveOccurred())
								var ok bool
								operatorSubscription, ok = subObj.(*operatorsv1alpha1.Subscription)
								Expect(ok).To(BeTrue())
								Expect(operatorSubscription).NotTo(BeNil())

								if operatorSubscription.Spec.Config.Resources.Limits.Cpu().Cmp(updatedCpuLimit) != 0 {
									// still hasn't been updated
									return fmt.Errorf("Operator Subscription in manifest work has not been " +
										"updated with the new limits")
								}

								return nil
							}, maxWait, interval).Should(Succeed())

							// The operatorSubscription has been updated - confirm we have the correct values
							Expect(operatorSubscription.Spec.Config.Resources.Limits.Cpu().
								Cmp(updatedCpuLimit)).To(Equal(0))
							Expect(operatorSubscription.Spec.Config.Resources.Limits.Memory().
								Cmp(updatedMemLimit)).To(Equal(0))
						})
					})
				})
			})

			Context("When the volsync ClusterManagementAddOn has a default deployment config w/ node selectors/tolerations", func() {
				var defaultAddonDeploymentConfig *addonv1alpha1.AddOnDeploymentConfig
				var mcAddon *addonv1alpha1.ManagedClusterAddOn
				var operatorSubscription *operatorsv1alpha1.Subscription
				var defaultNodePlacement *addonv1alpha1.NodePlacement

				myTolerationSeconds := int64(25)

				BeforeEach(func() {
					defaultNodePlacement = &addonv1alpha1.NodePlacement{
						NodeSelector: map[string]string{
							"testing":     "123",
							"specialnode": "very",
						},
						Tolerations: []corev1.Toleration{
							{
								Key:      "node.kubernetes.io/unreachable",
								Operator: corev1.TolerationOpExists,
								Effect:   corev1.TaintEffectPreferNoSchedule,
							},
							{
								Key:               "aaaaa",
								Operator:          corev1.TolerationOpExists,
								Effect:            corev1.TaintEffectNoExecute,
								TolerationSeconds: &myTolerationSeconds,
							},
						},
					}

					defaultAddonDeploymentConfig = createAddonDeploymentConfig(defaultNodePlacement)

					// Update the ClusterManagementAddOn before we create it to set a default deployment config
					clusterManagementAddon.Spec.SupportedConfigs[0].DefaultConfig = &addonv1alpha1.ConfigReferent{
						Name:      defaultAddonDeploymentConfig.GetName(),
						Namespace: defaultAddonDeploymentConfig.GetNamespace(),
					}

					// Create a ManagedClusterAddon for the mgd cluster
					mcAddon = &addonv1alpha1.ManagedClusterAddOn{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "volsync",
							Namespace: testManagedCluster.GetName(),
							Annotations: map[string]string{
								"operator-subscription-channel": "stable",
							},
						},
						Spec: addonv1alpha1.ManagedClusterAddOnSpec{}, // Setting spec to empty
					}
				})
				AfterEach(func() {
					cleanupAddonDeploymentConfig(defaultAddonDeploymentConfig, true)
				})

				JustBeforeEach(func() {
					// Create the managed cluster addon
					Expect(testK8sClient.Create(testCtx, mcAddon)).To(Succeed())

					manifestWork := &workv1.ManifestWork{}
					// The controller should create a ManifestWork for this ManagedClusterAddon
					Eventually(func() bool {
						allMwList := &workv1.ManifestWorkList{}
						Expect(testK8sClient.List(testCtx, allMwList,
							client.InNamespace(testManagedCluster.GetName()))).To(Succeed())

						for _, mw := range allMwList.Items {
							// addon-framework now creates manifestwork with "-0" prefix (to allow for
							// creating multiple manifestworks if the content is large - will not be the case
							// for VolSync - so we could alternatively just search for addon-volsync-deploy-0)
							if strings.HasPrefix(mw.GetName(), "addon-volsync-deploy") == true {
								manifestWork = &mw
								return true /* found the manifestwork */
							}
						}
						return false
					}, timeout, interval).Should(BeTrue())

					Expect(manifestWork).ToNot(BeNil())

					// Subscription
					subMF := manifestWork.Spec.Workload.Manifests[0]
					subObj, _, err := genericCodec.Decode(subMF.Raw, nil, nil)
					Expect(err).NotTo(HaveOccurred())
					var ok bool
					operatorSubscription, ok = subObj.(*operatorsv1alpha1.Subscription)
					Expect(ok).To(BeTrue())
					Expect(operatorSubscription).NotTo(BeNil())
					Expect(operatorSubscription.GetNamespace()).To(Equal("openshift-operators"))
					Expect(operatorSubscription.Spec.Package).To(Equal("volsync-product")) // This is the "name" in json

					// Check the annotation was still honoured
					Expect(operatorSubscription.Spec.Channel).To(Equal("stable"))
				})

				Context("When a ManagedClusterAddOn is created with no addonConfig specified (the default)", func() {
					It("Should create the sub in the manifestwork with the default node selector and tolerations", func() {
						Expect(operatorSubscription.Spec.Config).ToNot(BeNil())
						Expect(operatorSubscription.Spec.Config.NodeSelector).To(Equal(defaultNodePlacement.NodeSelector))
						Expect(operatorSubscription.Spec.Config.Tolerations).To(Equal(defaultNodePlacement.Tolerations))
					})
				})

				Context("When a ManagedClusterAddOn is created with addonConfig (node selectors and tolerations)", func() {
					var addonDeploymentConfig *addonv1alpha1.AddOnDeploymentConfig
					nodePlacement := &addonv1alpha1.NodePlacement{
						NodeSelector: map[string]string{
							"key1": "value1",
							"key2": "value2",
						},
						Tolerations: []corev1.Toleration{
							{
								Key:      "node.kubernetes.io/unreachable",
								Operator: corev1.TolerationOpExists,
								Effect:   corev1.TaintEffectNoSchedule,
							},
							{
								Key:      "mykey",
								Value:    "myvalue",
								Operator: corev1.TolerationOpEqual,
								Effect:   corev1.TaintEffectNoExecute,
							},
						},
					}
					BeforeEach(func() {
						addonDeploymentConfig = createAddonDeploymentConfig(nodePlacement)

						// Update the managedclusteraddon before we create it to add the addondeploymentconfig
						mcAddon.Spec.Configs = []addonv1alpha1.AddOnConfig{
							{
								ConfigGroupResource: addonv1alpha1.ConfigGroupResource{
									Group:    "addon.open-cluster-management.io",
									Resource: "addondeploymentconfigs",
								},
								ConfigReferent: addonv1alpha1.ConfigReferent{
									Name:      addonDeploymentConfig.GetName(),
									Namespace: addonDeploymentConfig.GetNamespace(),
								},
							},
						}
					})
					AfterEach(func() {
						cleanupAddonDeploymentConfig(addonDeploymentConfig, true)
					})

					It("Should create the sub in the manifestwork with the node selector and tolerations from "+
						" the managedclusteraddon, not the defaults", func() {
						Expect(operatorSubscription.Spec.Config).ToNot(BeNil())
						Expect(operatorSubscription.Spec.Config.NodeSelector).To(Equal(nodePlacement.NodeSelector))
						Expect(operatorSubscription.Spec.Config.Tolerations).To(Equal(nodePlacement.Tolerations))
					})
				})

				Context("When a default volsyncaddonconfig is also set in the clustermanagementconfig", func() {
					defaultCpuLimit, err := resource.ParseQuantity("1")
					Expect(err).NotTo(HaveOccurred())
					defaultMemLimit, err := resource.ParseQuantity("1.2Gi")
					Expect(err).NotTo(HaveOccurred())

					defaultCpuRequest, err := resource.ParseQuantity("0.02")
					Expect(err).NotTo(HaveOccurred())
					defaultMemRequest, err := resource.ParseQuantity("50Mi")
					Expect(err).NotTo(HaveOccurred())

					defaultVolSyncAddOnConfig := &volsyncaddonv1alpha1.VolSyncAddOnConfig{
						ObjectMeta: metav1.ObjectMeta{
							Name: "test-vsaddonconfig",
						},
						Spec: &volsyncaddonv1alpha1.VolSyncAddOnConfigSpec{
							SubscriptionCatalogSource: "my-test-catalog",
							SubscriptionConfig: &operatorsv1alpha1.SubscriptionConfig{
								NodeSelector: map[string]string{
									"fruit":  "pinapple",
									"planet": "jupiter",
								},

								Resources: &corev1.ResourceRequirements{
									Limits: corev1.ResourceList{
										corev1.ResourceCPU:    defaultCpuLimit,
										corev1.ResourceMemory: defaultMemLimit,
									},
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    defaultCpuRequest,
										corev1.ResourceMemory: defaultMemRequest,
									},
								},
							},
						},
					}

					BeforeEach(func() {
						// Create default VolSyncAddOnConfig
						// Put this in the same ns as the addondeploymentconfig, it'll get cleaned up too when we delete the ns at end of test
						defaultVolSyncAddOnConfig.Namespace = defaultAddonDeploymentConfig.GetNamespace()
						Expect(testK8sClient.Create(testCtx, defaultVolSyncAddOnConfig)).To(Succeed())

						// Update the ClusterManagementAddOn before we create it to set a default volsyncaddonconfig
						clusterManagementAddon.Spec.SupportedConfigs[1].DefaultConfig = &addonv1alpha1.ConfigReferent{
							Name:      defaultVolSyncAddOnConfig.GetName(),
							Namespace: defaultVolSyncAddOnConfig.GetNamespace(),
						}
					})

					It("Should create the sub in the manifestwork with settings from the volsyncAddOnConfig", func() {
						Expect(operatorSubscription.Spec.Config).ToNot(BeNil())

						// Settings from the default deploymentAddOnConfig
						Expect(operatorSubscription.Spec.Config.Tolerations).To(Equal(defaultNodePlacement.Tolerations))
						Expect(operatorSubscription.Spec.Config.NodeSelector).To(Equal(defaultNodePlacement.NodeSelector))

						//
						// Settings from the volsyncaddonconfig
						//
						// Check cpu limit was set properly
						Expect(operatorSubscription.Spec.Config.Resources).NotTo(BeNil())
						Expect(operatorSubscription.Spec.Config.Resources.Limits).NotTo(BeNil())
						Expect(operatorSubscription.Spec.Config.Resources.Limits.Cpu().Cmp(defaultCpuLimit)).To(Equal(0))
						Expect(operatorSubscription.Spec.Config.Resources.Limits.Memory().Cmp(defaultMemLimit)).To(Equal(0))
						// Check cpu and mem requests were set properly
						Expect(operatorSubscription.Spec.Config.Resources.Requests).NotTo(BeNil())
						Expect(operatorSubscription.Spec.Config.Resources.Requests.Cpu().Cmp(defaultCpuRequest)).To(Equal(0))
						Expect(operatorSubscription.Spec.Config.Resources.Requests.Memory().Cmp(defaultMemRequest)).To(Equal(0))
					})
				})
			})
		})
	})

	Context("When a ManagedClusterExists with the install volsync addon label", func() {
		var testManagedCluster *clusterv1.ManagedCluster
		var testManagedClusterNamespace *corev1.Namespace

		BeforeEach(func() {
			// Create a managed cluster CR to use for this test - with volsync addon install label
			testManagedCluster = &clusterv1.ManagedCluster{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "addon-mgdcluster-",
					Labels: map[string]string{
						"vendor": "OpenShift",
						controllers.ManagedClusterInstallVolSyncLabel: controllers.ManagedClusterInstallVolSyncLabelValue,
					},
				},
			}

			Expect(testK8sClient.Create(testCtx, testManagedCluster)).To(Succeed())
			Expect(testManagedCluster.Name).NotTo(BeEmpty())

			// Fake the status of the mgd cluster to be available
			Eventually(func() error {
				err := testK8sClient.Get(testCtx, client.ObjectKeyFromObject(testManagedCluster), testManagedCluster)
				if err != nil {
					return err
				}

				clusterAvailableCondition := metav1.Condition{
					Type:    clusterv1.ManagedClusterConditionAvailable,
					Status:  metav1.ConditionTrue,
					Reason:  "testupdate",
					Message: "faking cluster available for test",
				}
				meta.SetStatusCondition(&testManagedCluster.Status.Conditions, clusterAvailableCondition)

				return testK8sClient.Status().Update(testCtx, testManagedCluster)
			}, timeout, interval).Should(Succeed())

			// Create a matching namespace for this managed cluster
			// (namespace with name=managedclustername is expected to exist on the hub)
			testManagedClusterNamespace = &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: testManagedCluster.GetName(),
				},
			}
			Expect(testK8sClient.Create(testCtx, testManagedClusterNamespace)).To(Succeed())
		})

		It("Should automatically create a ManagedClusterAddon for volsync in the managedcluster namespace", func() {
			vsAddon := &addonv1alpha1.ManagedClusterAddOn{}

			// The controller should create a volsync ManagedClusterAddOn in the ManagedCluster NS
			Eventually(func() error {
				return testK8sClient.Get(testCtx, types.NamespacedName{
					Name:      "volsync",
					Namespace: testManagedCluster.GetName(),
				}, vsAddon)
			}, timeout, interval).Should(Succeed())

			// This ns is now the default in the mcao crd so will be used since we don't set it - note we ignore
			// this and use openshift-operators (see the created subscription)
			Expect(vsAddon.Spec.InstallNamespace).To(Equal("open-cluster-management-agent-addon"))
		})
	})
})

var _ = Describe("Addon Status Update Tests", func() {
	logger := zap.New(zap.UseDevMode(true), zap.WriteTo(GinkgoWriter))

	// Make sure a ClusterManagementAddOn exists for volsync or addon-framework will not reconcile
	// VolSync ManagedClusterAddOns
	var clusterManagementAddon *addonv1alpha1.ClusterManagementAddOn

	BeforeEach(func() {
		// clustermanagementaddon (this is a global resource)
		clusterManagementAddon = &addonv1alpha1.ClusterManagementAddOn{
			ObjectMeta: metav1.ObjectMeta{
				Name: "volsync",
			},
			Spec: addonv1alpha1.ClusterManagementAddOnSpec{
				AddOnMeta: addonv1alpha1.AddOnMeta{
					DisplayName: "VolSync",
					Description: "VolSync",
				},
				SupportedConfigs: []addonv1alpha1.ConfigMeta{
					{
						ConfigGroupResource: addonv1alpha1.ConfigGroupResource{
							Group:    "addon.open-cluster-management.io",
							Resource: "addondeploymentconfigs",
						},
					},
					{
						ConfigGroupResource: addonv1alpha1.ConfigGroupResource{
							Group:    "addon.open-cluster-management.io",
							Resource: "volsyncaddonconfigs",
						},
					},
				},
				InstallStrategy: addonv1alpha1.InstallStrategy{
					Type: addonv1alpha1.AddonInstallStrategyManual,
				},
			},
		}
	})
	AfterEach(func() {
		Expect(testK8sClient.Delete(testCtx, clusterManagementAddon)).To(Succeed())
	})

	JustBeforeEach(func() {
		// Create the clustermanagementaddon here so tests can modify it in their BeforeEach()
		// before we create it
		Expect(testK8sClient.Create(testCtx, clusterManagementAddon)).To(Succeed())
	})

	Context("When a ManagedClusterExists", func() {
		var testManagedCluster *clusterv1.ManagedCluster
		var testManagedClusterNamespace *corev1.Namespace

		BeforeEach(func() {
			// Create a managed cluster CR to use for this test
			testManagedCluster = &clusterv1.ManagedCluster{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "addon-inst-mgdcluster-",
					Labels: map[string]string{
						"vendor": "OpenShift",
					},
				},
			}
		})

		JustBeforeEach(func() {
			Expect(testK8sClient.Create(testCtx, testManagedCluster)).To(Succeed())
			Expect(testManagedCluster.Name).NotTo(BeEmpty())

			// Fake the status of the mgd cluster to be available
			Eventually(func() error {
				err := testK8sClient.Get(testCtx, client.ObjectKeyFromObject(testManagedCluster), testManagedCluster)
				if err != nil {
					return err
				}

				clusterAvailableCondition := metav1.Condition{
					Type:    clusterv1.ManagedClusterConditionAvailable,
					Status:  metav1.ConditionTrue,
					Reason:  "testupdate",
					Message: "faking cluster available for test",
				}
				meta.SetStatusCondition(&testManagedCluster.Status.Conditions, clusterAvailableCondition)

				return testK8sClient.Status().Update(testCtx, testManagedCluster)
			}, timeout, interval).Should(Succeed())

			// Create a matching namespace for this managed cluster
			// (namespace with name=managedclustername is expected to exist on the hub)
			testManagedClusterNamespace = &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: testManagedCluster.GetName(),
				},
			}
			Expect(testK8sClient.Create(testCtx, testManagedClusterNamespace)).To(Succeed())
		})

		Context("When a ManagedClusterAddon for this addon is created", func() {
			var mcAddon *addonv1alpha1.ManagedClusterAddOn
			JustBeforeEach(func() {
				// Create a ManagedClusterAddon for the mgd cluster
				mcAddon = &addonv1alpha1.ManagedClusterAddOn{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "volsync",
						Namespace: testManagedClusterNamespace.GetName(),
					},
					Spec: addonv1alpha1.ManagedClusterAddOnSpec{},
				}
				Expect(testK8sClient.Create(testCtx, mcAddon)).To(Succeed())
			})

			Context("When the managed cluster is an OpenShiftCluster and manifestwork is available", func() {
				JustBeforeEach(func() {
					// The controller should create a ManifestWork for this ManagedClusterAddon
					// Fake out that the ManifestWork is applied and available
					Eventually(func() error {
						var manifestWork *workv1.ManifestWork

						allMwList := &workv1.ManifestWorkList{}
						Expect(testK8sClient.List(testCtx, allMwList,
							client.InNamespace(testManagedCluster.GetName()))).To(Succeed())

						for _, mw := range allMwList.Items {
							if strings.HasPrefix(mw.GetName(), "addon-volsync-deploy") == true {
								manifestWork = &mw
								break
							}
						}

						if manifestWork == nil {
							return fmt.Errorf("Did not find the manifestwork with prefix addon-volsync-deploy")
						}

						workAppliedCondition := metav1.Condition{
							Type:    workv1.WorkApplied,
							Status:  metav1.ConditionTrue,
							Reason:  "testupdate",
							Message: "faking applied for test",
						}
						meta.SetStatusCondition(&manifestWork.Status.Conditions, workAppliedCondition)

						workAvailableCondition := metav1.Condition{
							Type:    workv1.WorkAvailable,
							Status:  metav1.ConditionTrue,
							Reason:  "testupdate",
							Message: "faking avilable for test",
						}
						meta.SetStatusCondition(&manifestWork.Status.Conditions, workAvailableCondition)

						return testK8sClient.Status().Update(testCtx, manifestWork)
					}, timeout, interval).Should(Succeed())
				})

				Context("When the manifestwork statusFeedback is not available", func() {
					It("Should set the ManagedClusterAddon status to unknown", func() {
						var statusCondition *metav1.Condition
						Eventually(func() bool {
							err := testK8sClient.Get(testCtx, types.NamespacedName{
								Name:      "volsync",
								Namespace: testManagedClusterNamespace.GetName(),
							}, mcAddon)
							if err != nil {
								return false
							}

							statusCondition = meta.FindStatusCondition(mcAddon.Status.Conditions,
								addonv1alpha1.ManagedClusterAddOnConditionAvailable)
							return statusCondition != nil
						}, timeout, interval).Should(BeTrue())

						Expect(statusCondition.Reason).To(Equal("NoProbeResult"))
						Expect(statusCondition.Status).To(Equal(metav1.ConditionUnknown))
					})
				})

				Context("When the manifestwork statusFeedback is returned with a bad value", func() {
					JustBeforeEach(func() {
						Eventually(func() error {
							// Update the manifestwork to set the statusfeedback to a bad value
							var manifestWork *workv1.ManifestWork

							allMwList := &workv1.ManifestWorkList{}
							Expect(testK8sClient.List(testCtx, allMwList,
								client.InNamespace(testManagedCluster.GetName()))).To(Succeed())

							for _, mw := range allMwList.Items {
								if strings.HasPrefix(mw.GetName(), "addon-volsync-deploy") == true {
									manifestWork = &mw
									break
								}
							}

							if manifestWork == nil {
								return fmt.Errorf("Did not find the manifestwork with prefix addon-volsync-deploy")
							}

							manifestWork.Status.ResourceStatus =
								manifestWorkResourceStatusWithSubscriptionInstalledCSVFeedBack("notinstalled")

							return testK8sClient.Status().Update(testCtx, manifestWork)
						}, timeout, interval).Should(Succeed())
					})

					It("Should set the ManagedClusterAddon status to unknown", func() {
						var statusCondition *metav1.Condition
						Eventually(func() bool {
							err := testK8sClient.Get(testCtx, types.NamespacedName{
								Name:      "volsync",
								Namespace: testManagedClusterNamespace.GetName(),
							}, mcAddon)
							if err != nil {
								return false
							}

							statusCondition = meta.FindStatusCondition(mcAddon.Status.Conditions,
								addonv1alpha1.ManagedClusterAddOnConditionAvailable)
							return statusCondition.Reason == "ProbeUnavailable"
						}, timeout, interval).Should(BeTrue())

						Expect(statusCondition.Reason).To(Equal("ProbeUnavailable"))
						Expect(statusCondition.Status).To(Equal(metav1.ConditionFalse))
						Expect(statusCondition.Message).To(ContainSubstring("Probe addon unavailable with err"))
						Expect(statusCondition.Message).To(ContainSubstring("unexpected installedCSV value"))
					})
				})

				Context("When the manifestwork statusFeedback is returned with a correct installed value", func() {
					JustBeforeEach(func() {
						Eventually(func() error {
							// Update the manifestwork to set the statusfeedback to a bad value
							var manifestWork *workv1.ManifestWork

							allMwList := &workv1.ManifestWorkList{}
							Expect(testK8sClient.List(testCtx, allMwList,
								client.InNamespace(testManagedCluster.GetName()))).To(Succeed())

							for _, mw := range allMwList.Items {
								if strings.HasPrefix(mw.GetName(), "addon-volsync-deploy") == true {
									manifestWork = &mw
									break
								}
							}

							if manifestWork == nil {
								return fmt.Errorf("Did not find the manifestwork with prefix addon-volsync-deploy")
							}

							manifestWork.Status.ResourceStatus =
								manifestWorkResourceStatusWithSubscriptionInstalledCSVFeedBack("volsync-product.v0.4.0")

							return testK8sClient.Status().Update(testCtx, manifestWork)
						}, timeout, interval).Should(Succeed())

					})

					It("Should set the ManagedClusterAddon status to available", func() {
						var statusCondition *metav1.Condition
						Eventually(func() bool {
							err := testK8sClient.Get(testCtx, types.NamespacedName{
								Name:      "volsync",
								Namespace: testManagedClusterNamespace.GetName(),
							}, mcAddon)
							if err != nil {
								return false
							}

							statusCondition = meta.FindStatusCondition(mcAddon.Status.Conditions,
								addonv1alpha1.ManagedClusterAddOnConditionAvailable)
							return statusCondition.Reason == "ProbeAvailable"
						}, timeout, interval).Should(BeTrue())

						logger.Info("status condition", "statusCondition", statusCondition)

						Expect(statusCondition.Reason).To(Equal("ProbeAvailable"))
						Expect(statusCondition.Status).To(Equal(metav1.ConditionTrue))
						//TODO: should contain volsync in msg (i.e. "volsync addon is available"), requires change
						// from addon-framework
						Expect(statusCondition.Message).To(Equal("Addon is available"))
					})
				})
			})

			Context("When the managed cluster is not an OpenShift cluster", func() {
				BeforeEach(func() {
					// remove labels from the managedcluster resource before it's created
					// to simulate a "non-OpenShift" cluster
					testManagedCluster.Labels = map[string]string{}
				})

				It("ManagedClusterAddOn status should not be successful", func() {
					var statusCondition *metav1.Condition
					Consistently(func() *metav1.Condition {
						err := testK8sClient.Get(testCtx, types.NamespacedName{
							Name:      "volsync",
							Namespace: testManagedClusterNamespace.GetName(),
						}, mcAddon)
						if err != nil {
							return nil
						}

						logger.Info("status should not be successful", "mcAddon.Status.Conditions", mcAddon.Status.Conditions)
						statusCondition = meta.FindStatusCondition(mcAddon.Status.Conditions,
							addonv1alpha1.ManagedClusterAddOnConditionAvailable)
						return statusCondition
						// addon-framework no longer sets a condition - simply doesn't declare it as available
					}, fiveSeconds, interval).Should(BeNil())

					/*
						Expect(statusCondition.Reason).To(Equal("WorkNotFound")) // We didn't deploy any manifests
						Expect(statusCondition.Status).To(Equal(metav1.ConditionUnknown))
					*/
				})
			})
		})
	})
})

// Tests of the function that creates the subscription - make sure settings are all set from the correct place
var _ = Describe("VolSync subscription creation tests", func() {
	vsAgent := controllers.NewVolSyncAgent(nil /* not used in these tests */, testK8sClient)

	var volsyncSubscription *operatorsv1alpha1.Subscription

	var mcAddOn *addonv1alpha1.ManagedClusterAddOn
	var vsAddOnConfig *volsyncaddonv1alpha1.VolSyncAddOnConfig
	var deploymentConfig *addonv1alpha1.AddOnDeploymentConfig

	BeforeEach(func() {
		mcAddOn = &addonv1alpha1.ManagedClusterAddOn{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "volsync",
				Namespace: "mgd-cluster-abc-123",
			},
			Spec: addonv1alpha1.ManagedClusterAddOnSpec{}, // Setting spec to empty,
		}
		vsAddOnConfig = nil    // Reset for each test
		deploymentConfig = nil // Reset for each test
	})

	JustBeforeEach(func() {
		var vsAddOnConfigSpec *volsyncaddonv1alpha1.VolSyncAddOnConfigSpec
		if vsAddOnConfig != nil {
			vsAddOnConfigSpec = vsAddOnConfig.Spec
		}
		var deploymentConfigValues addonfactory.Values
		var err error
		if deploymentConfig != nil {
			deploymentConfigValues, err = addonfactory.ToAddOnDeploymentConfigValues(*deploymentConfig)
			Expect(err).NotTo(HaveOccurred())
		}

		volsyncSubscription, err = vsAgent.VolsyncOperatorSubscription(mcAddOn, vsAddOnConfigSpec, deploymentConfigValues)
		Expect(err).NotTo(HaveOccurred())
		Expect(volsyncSubscription).NotTo(BeNil())

		// These settings should always be set
		Expect(volsyncSubscription.GetName()).To(Equal(controllers.OperatorName))
		Expect(volsyncSubscription.GetNamespace()).To(Equal(controllers.GlobalOperatorInstallNamespace))
		Expect(volsyncSubscription.Spec).NotTo(BeNil())
		Expect(volsyncSubscription.Spec.Package).To(Equal(controllers.OperatorName))
	})

	Context("When no volsyncAddOnConfig or deploymentConfig is set", func() {
		Context("When no annotations are set", func() {
			It("subscription should use defaults", func() {
				// Check all defaults are set properly
				Expect(volsyncSubscription.Spec.CatalogSource).To(Equal(controllers.DefaultCatalogSource))
				Expect(volsyncSubscription.Spec.CatalogSourceNamespace).To(Equal(controllers.DefaultCatalogSourceNamespace))
				Expect(volsyncSubscription.Spec.Channel).To(Equal(controllers.DefaultChannel))
				Expect(volsyncSubscription.Spec.InstallPlanApproval).To(Equal(
					operatorsv1alpha1.Approval(controllers.DefaultInstallPlanApproval)))
				Expect(volsyncSubscription.Spec.StartingCSV).To(Equal(controllers.DefaultStartingCSV))
				// spec.config should be nil by default
				Expect(volsyncSubscription.Spec.Config).To(BeNil())
			})
		})

		Context("When annotations are set", func() {
			BeforeEach(func() {
				mcAddOn.Annotations = map[string]string{
					controllers.AnnotationChannelOverride:             "test-channel",
					controllers.AnnotationInstallPlanApprovalOverride: "Manual",
				}
			})

			It("subscription should use the annotated values", func() {
				// Should be taken from our annotations on the managedclusteraddon
				Expect(volsyncSubscription.Spec.Channel).To(Equal("test-channel"))
				Expect(volsyncSubscription.Spec.InstallPlanApproval).To(Equal(
					operatorsv1alpha1.Approval("Manual")))
				// Other values should still be defaults
				Expect(volsyncSubscription.Spec.CatalogSource).To(Equal(controllers.DefaultCatalogSource))
			})
		})
	})

	Context("When a volsyncAddonConfig is used", func() {
		BeforeEach(func() {
			cpuLimit, err := resource.ParseQuantity("50m")
			Expect(err).NotTo(HaveOccurred())

			vsAddOnConfig = &volsyncaddonv1alpha1.VolSyncAddOnConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vsaddonconfig",
					Namespace: "some-ns-name",
				},
				Spec: &volsyncaddonv1alpha1.VolSyncAddOnConfigSpec{
					SubscriptionChannel: "channel-picked-by-vsaddonconfig",
					SubscriptionConfig: &operatorsv1alpha1.SubscriptionConfig{
						NodeSelector: map[string]string{
							"picknode": "picklabel",
							"label2":   "label2value",
						},
						Tolerations: []corev1.Toleration{
							{
								Key:      "node.kubernetes.io/unreachable",
								Operator: corev1.TolerationOpExists,
								Effect:   corev1.TaintEffectNoSchedule,
							},
						},
						Resources: &corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceCPU: cpuLimit,
							},
						},
						Env: []corev1.EnvVar{
							{
								Name:  "FRUIT",
								Value: "bananas",
							},
							{
								Name:  "VEGETABLE",
								Value: "broccoli",
							},
						},
					},
				},
			}
		})

		It("Subscription should set resource and env vars from the vsAddonConfig", func() {
			// For all cases here, env vars and resource requirements should come from vsAddOnConfig
			// (deploymentConfig does not allow to override env vars or resourcerequirements)

			// Resource requirements
			Expect(volsyncSubscription.Spec.Config.Resources).To(Equal(vsAddOnConfig.Spec.SubscriptionConfig.Resources))

			// Check that env vars are set
			Expect(volsyncSubscription.Spec.Config.Env[0]).To(Equal(corev1.EnvVar{Name: "FRUIT", Value: "bananas"}))
			Expect(volsyncSubscription.Spec.Config.Env[1]).To(Equal(corev1.EnvVar{Name: "VEGETABLE", Value: "broccoli"}))
		})

		Context("When no annotations are set", func() {
			It("subscription should use defaults", func() {
				// Defaults
				Expect(volsyncSubscription.Spec.CatalogSource).To(Equal(controllers.DefaultCatalogSource))
				Expect(volsyncSubscription.Spec.CatalogSourceNamespace).To(Equal(controllers.DefaultCatalogSourceNamespace))
				Expect(volsyncSubscription.Spec.InstallPlanApproval).To(Equal(
					operatorsv1alpha1.Approval(controllers.DefaultInstallPlanApproval)))
				Expect(volsyncSubscription.Spec.StartingCSV).To(Equal(controllers.DefaultStartingCSV))

				// Should come from vsAddOnConfig
				Expect(volsyncSubscription.Spec.Channel).To(Equal(vsAddOnConfig.Spec.SubscriptionChannel))
				Expect(volsyncSubscription.Spec.Config).To(Equal(vsAddOnConfig.Spec.SubscriptionConfig))

			})
		})

		Context("When annotations are set", func() {
			BeforeEach(func() {
				mcAddOn.Annotations = map[string]string{
					controllers.AnnotationChannelOverride:             "test-channel",
					controllers.AnnotationInstallPlanApprovalOverride: "Manual",
				}
			})

			It("subscription should use the vsAddonConfig ahead of annotated values", func() {
				// Should be taken from our annotations on the managedclusteraddon since not set in vsAddOnConfig
				Expect(volsyncSubscription.Spec.InstallPlanApproval).To(Equal(
					operatorsv1alpha1.Approval("Manual")))

				// Should be taken from the vsAddonConfig, overriding value from annotation
				Expect(volsyncSubscription.Spec.Channel).To(Equal(vsAddOnConfig.Spec.SubscriptionChannel))

				// Other values should still be defaults
				Expect(volsyncSubscription.Spec.CatalogSource).To(Equal(controllers.DefaultCatalogSource))
			})
		})

		Context("when no deploymentConfig is used", func() {
			It("Subscription nodeToleration, nodeResources should come from the vsAddonConfig", func() {
				Expect(volsyncSubscription.Spec.Config.NodeSelector).To(Equal(vsAddOnConfig.Spec.SubscriptionConfig.NodeSelector))
				Expect(volsyncSubscription.Spec.Config.Tolerations).To(Equal(vsAddOnConfig.Spec.SubscriptionConfig.Tolerations))
			})
		})

		Context("When a deploymentConfig is used", func() {
			BeforeEach(func() {
				deploymentConfig = &addonv1alpha1.AddOnDeploymentConfig{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "my-deployment-config",
						Namespace: "doesntmatter-ns-name",
					},
					Spec: addonv1alpha1.AddOnDeploymentConfigSpec{
						NodePlacement: &addonv1alpha1.NodePlacement{
							NodeSelector: map[string]string{
								"noderole": "test",
								"planet":   "earth",
							},
						},
					},
				}
			})

			It("Subscription should use values from the deploymentConfig before the vsAddonConfig", func() {
				// NodeSelector was specified in the deploymentConfig so it should be used
				Expect(volsyncSubscription.Spec.Config.NodeSelector).To(Equal(deploymentConfig.Spec.NodePlacement.NodeSelector))

				// The deploymentConfig didn't specify tolerations, so they should still be coming from
				// the vsAddOnConfig
				Expect(volsyncSubscription.Spec.Config.Tolerations).To(Equal(vsAddOnConfig.Spec.SubscriptionConfig.Tolerations))
			})
		})
	})

	Context("When no vsAddOnConfig is used", func() {
		Context("When a deploymentConfig is used", func() {
			BeforeEach(func() {
				deploymentConfig = &addonv1alpha1.AddOnDeploymentConfig{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "my-test-depl-config",
						Namespace: "my-ns-name",
					},
					Spec: addonv1alpha1.AddOnDeploymentConfigSpec{
						NodePlacement: &addonv1alpha1.NodePlacement{
							NodeSelector: map[string]string{
								"node-type": "biggest",
								"planet":    "jupiter",
							},
							Tolerations: []corev1.Toleration{
								{
									Key:      "node.kubernetes.io/unreachable",
									Operator: corev1.TolerationOpExists,
									Effect:   corev1.TaintEffectNoSchedule,
								},
							},
						},
					},
				}
			})

			It("Subscription should use values from the deploymentConfig", func() {
				// NodeSelector was specified in the deploymentConfig so it should be used
				Expect(volsyncSubscription.Spec.Config.NodeSelector).To(Equal(deploymentConfig.Spec.NodePlacement.NodeSelector))
				// Tolerations were specified in the deploymentConfig so they should be used
				Expect(volsyncSubscription.Spec.Config.Tolerations).To(Equal(deploymentConfig.Spec.NodePlacement.Tolerations))
			})
		})
	})
})

func manifestWorkResourceStatusWithSubscriptionInstalledCSVFeedBack(installedCSVValue string) workv1.ManifestResourceStatus {
	return workv1.ManifestResourceStatus{
		Manifests: []workv1.ManifestCondition{
			{
				ResourceMeta: workv1.ManifestResourceMeta{
					Group:     "operators.coreos.com",
					Kind:      "Subscription",
					Name:      "volsync-product",
					Namespace: "openshift-operators",
					Resource:  "subscriptions",
					Version:   "v1alpha1",
				},
				StatusFeedbacks: workv1.StatusFeedbackResult{
					Values: []workv1.FeedbackValue{
						{
							Name: "installedCSV",
							Value: workv1.FieldValue{
								Type:   "String",
								String: &installedCSVValue,
							},
						},
					},
				},
				Conditions: []metav1.Condition{},
			},
		},
	}
}

func createAddonDeploymentConfig(nodePlacement *addonv1alpha1.NodePlacement) *addonv1alpha1.AddOnDeploymentConfig {
	// Create a ns to host the addondeploymentconfig
	// These can be accessed globally, so could be in the mgd cluster namespace
	// but, creating a new ns for each one to keep the tests simple
	tempNamespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-temp-",
		},
	}
	Expect(testK8sClient.Create(testCtx, tempNamespace)).To(Succeed())

	// Create an addonDeploymentConfig
	customAddonDeploymentConfig := &addonv1alpha1.AddOnDeploymentConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-deployment-config-1",
			Namespace: tempNamespace.GetName(),
		},
		Spec: addonv1alpha1.AddOnDeploymentConfigSpec{
			NodePlacement: nodePlacement,
		},
	}
	Expect(testK8sClient.Create(testCtx, customAddonDeploymentConfig)).To(Succeed())

	return customAddonDeploymentConfig
}

func cleanupAddonDeploymentConfig(addonDeploymentConfig *addonv1alpha1.AddOnDeploymentConfig, cleanupNamespace bool) {
	// Assumes the addondeploymentconfig has its own namespace - cleans up the addondeploymentconfig
	// and optionally the namespace as well
	nsName := addonDeploymentConfig.GetNamespace()
	Expect(testK8sClient.Delete(testCtx, addonDeploymentConfig)).To(Succeed())
	if cleanupNamespace {
		ns := &corev1.Namespace{}
		Expect(testK8sClient.Get(testCtx, types.NamespacedName{Name: nsName}, ns)).To(Succeed())
		Expect(testK8sClient.Delete(testCtx, ns)).To(Succeed())
	}
}
