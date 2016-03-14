package watcher_test

import (
	"errors"
	"os"
	"time"

	fake_routing_table "github.com/cloudfoundry-incubator/cf-tcp-router/routing_table/fakes"
	"github.com/cloudfoundry-incubator/cf-tcp-router/watcher"
	"github.com/cloudfoundry-incubator/routing-api"
	"github.com/cloudfoundry-incubator/routing-api/fake_routing_api"
	"github.com/cloudfoundry-incubator/routing-api/models"
	testUaaClient "github.com/cloudfoundry-incubator/uaa-go-client/fakes"
	"github.com/cloudfoundry-incubator/uaa-go-client/schema"
	"github.com/tedsuo/ifrit"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
)

var _ = Describe("Watcher", func() {
	const (
		routerGroupGuid = "rtrGrp0001"
	)
	var (
		eventSource      *fake_routing_api.FakeTcpEventSource
		routingApiClient *fake_routing_api.FakeClient
		uaaClient        *testUaaClient.FakeClient
		testWatcher      *watcher.Watcher
		process          ifrit.Process
		eventChannel     chan routing_api.TcpEvent
		errorChannel     chan error
		syncChannel      chan struct{}
		updater          *fake_routing_table.FakeUpdater
	)

	BeforeEach(func() {
		eventSource = new(fake_routing_api.FakeTcpEventSource)
		routingApiClient = new(fake_routing_api.FakeClient)
		updater = new(fake_routing_table.FakeUpdater)
		uaaClient = &testUaaClient.FakeClient{}
		token := &schema.Token{
			AccessToken: "access_token",
			ExpiresIn:   5,
		}
		uaaClient.FetchTokenReturns(token, nil)

		routingApiClient.SubscribeToTcpEventsReturns(eventSource, nil)
		syncChannel = make(chan struct{})
		testWatcher = watcher.New(routingApiClient, updater, uaaClient, 1, syncChannel, logger)

		eventChannel = make(chan routing_api.TcpEvent)
		errorChannel = make(chan error)

		eventSource.CloseStub = func() error {
			errorChannel <- errors.New("closed")
			return nil
		}

		eventSource.NextStub = func() (routing_api.TcpEvent, error) {
			select {
			case event := <-eventChannel:
				return event, nil
			case err := <-errorChannel:
				return routing_api.TcpEvent{}, err
			}
		}
	})

	JustBeforeEach(func() {
		process = ifrit.Invoke(testWatcher)
	})

	AfterEach(func() {
		process.Signal(os.Interrupt)
		Eventually(process.Wait()).Should(Receive())
		Eventually(logger).Should(gbytes.Say("test.watcher.stopping"))
	})

	Context("handle UpsertEvent", func() {
		var (
			tcpEvent routing_api.TcpEvent
		)

		JustBeforeEach(func() {
			tcpEvent = routing_api.TcpEvent{
				TcpRouteMapping: models.TcpRouteMapping{
					TcpRoute: models.TcpRoute{
						RouterGroupGuid: routerGroupGuid,
						ExternalPort:    61000,
					},
					HostPort: 5222,
					HostIP:   "some-ip-1",
				},
				Action: "Upsert",
			}
			eventChannel <- tcpEvent
		})

		It("calls updater HandleEvent", func() {
			Eventually(updater.HandleEventCallCount, 5*time.Second, 300*time.Millisecond).Should(Equal(1))
			upsertEvent := updater.HandleEventArgsForCall(0)
			Expect(upsertEvent).Should(Equal(tcpEvent))
		})
	})

	Context("handle DeleteEvent", func() {
		var (
			tcpEvent routing_api.TcpEvent
		)

		JustBeforeEach(func() {
			tcpEvent = routing_api.TcpEvent{
				TcpRouteMapping: models.TcpRouteMapping{
					TcpRoute: models.TcpRoute{
						RouterGroupGuid: routerGroupGuid,
						ExternalPort:    61000,
					},
					HostPort: 5222,
					HostIP:   "some-ip-1",
				},
				Action: "Delete",
			}
			eventChannel <- tcpEvent
		})

		It("calls updater HandleEvent", func() {
			Eventually(updater.HandleEventCallCount, 5*time.Second, 300*time.Millisecond).Should(Equal(1))
			deleteEvent := updater.HandleEventArgsForCall(0)
			Expect(deleteEvent).Should(Equal(tcpEvent))
		})
	})

	Context("handle Sync Event", func() {
		JustBeforeEach(func() {
			syncChannel <- struct{}{}
		})

		It("calls updater Sync", func() {
			Eventually(updater.SyncCallCount, 5*time.Second, 300*time.Millisecond).Should(Equal(1))
		})
	})

	Context("when eventSource returns error", func() {
		JustBeforeEach(func() {
			Eventually(routingApiClient.SubscribeToTcpEventsCallCount).Should(Equal(1))
			errorChannel <- errors.New("buzinga...")
		})

		It("resubscribes to SSE from routing api", func() {
			Eventually(routingApiClient.SubscribeToTcpEventsCallCount, 5*time.Second, 300*time.Millisecond).Should(Equal(2))
			Eventually(logger).Should(gbytes.Say("test.watcher.failed-getting-next-tcp-routing-event"))
		})
	})

	Context("when subscribe to events fails", func() {
		var (
			routingApiErrChannel chan error
		)
		BeforeEach(func() {
			routingApiErrChannel = make(chan error)

			routingApiClient.SubscribeToTcpEventsStub = func() (routing_api.TcpEventSource, error) {
				select {
				case err := <-routingApiErrChannel:
					if err != nil {
						return nil, err
					}
				}
				return eventSource, nil
			}

			testWatcher = watcher.New(routingApiClient, updater, uaaClient, 1, syncChannel, logger)
		})

		Context("with error other than unauthorized", func() {
			It("uses the cached token and retries to subscribe", func() {
				Eventually(uaaClient.FetchTokenCallCount, 5*time.Second, 1*time.Second).Should(Equal(1))
				Expect(uaaClient.FetchTokenArgsForCall(0)).To(BeTrue())
				routingApiErrChannel <- errors.New("kaboom")
				close(routingApiErrChannel)
				Eventually(routingApiClient.SubscribeToTcpEventsCallCount, 5*time.Second, 1*time.Second).Should(Equal(2))
				Eventually(logger).Should(gbytes.Say("test.watcher.failed-subscribing-to-tcp-routing-events"))
				Eventually(uaaClient.FetchTokenCallCount, 5*time.Second, 1*time.Second).Should(Equal(2))
				Expect(uaaClient.FetchTokenArgsForCall(1)).To(BeTrue())
			})
		})

		Context("with unauthorized error", func() {
			It("fetches a new token and retries to subscribe", func() {
				Eventually(uaaClient.FetchTokenCallCount, 5*time.Second, 1*time.Second).Should(Equal(1))
				Expect(uaaClient.FetchTokenArgsForCall(0)).To(BeTrue())
				routingApiErrChannel <- errors.New("unauthorized")
				Eventually(routingApiClient.SubscribeToTcpEventsCallCount, 5*time.Second, 1*time.Second).Should(Equal(2))
				Eventually(logger).Should(gbytes.Say("test.watcher.failed-subscribing-to-tcp-routing-events"))
				Eventually(uaaClient.FetchTokenCallCount, 5*time.Second, 1*time.Second).Should(Equal(2))
				Expect(uaaClient.FetchTokenArgsForCall(1)).To(BeFalse())

				By("resumes to use cache token for subsequent errors")
				routingApiErrChannel <- errors.New("kaboom")
				close(routingApiErrChannel)
				Eventually(routingApiClient.SubscribeToTcpEventsCallCount, 5*time.Second, 1*time.Second).Should(Equal(3))
				Eventually(logger).Should(gbytes.Say("test.watcher.failed-subscribing-to-tcp-routing-events"))
				Eventually(uaaClient.FetchTokenCallCount, 5*time.Second, 1*time.Second).Should(Equal(3))
				Expect(uaaClient.FetchTokenArgsForCall(2)).To(BeTrue())
			})
		})
	})

	Context("when the token fetcher returns an error", func() {
		BeforeEach(func() {
			uaaClient.FetchTokenReturns(nil, errors.New("token fetcher error"))
		})

		It("returns an error", func() {
			Eventually(logger).Should(gbytes.Say("test.watcher.error-fetching-token"))
			Eventually(uaaClient.FetchTokenCallCount, 5*time.Second, 1*time.Second).Should(BeNumerically(">", 2))
		})
	})

})
