package workers

import (
	"github.com/go-redis/redis"
	"os"
	"sync"

	"github.com/pborman/uuid"
)

type Manager struct {
	uuid     string
	opts     Options
	schedule *scheduledWorker
	workers  []*worker
	lock     sync.Mutex
	signal   chan os.Signal
	running  bool

	beforeStartHooks []func()
	duringDrainHooks []func()
}

func NewManager(options Options) (*Manager, error) {
	options, err := processOptions(options)
	if err != nil {
		return nil, err
	}
	return &Manager{
		uuid: uuid.New(),
		opts: options,
	}, nil
}

func (m *Manager) GetRedisClient() *redis.Client {
	return m.opts.client
}

func (m *Manager) AddWorker(queue string, concurrency int, job JobFunc, mids ...MiddlewareFunc) {
	m.lock.Lock()
	defer m.lock.Unlock()

	middlewareQueueName := m.opts.Namespace + queue
	if len(mids) == 0 {
		job = DefaultMiddlewares().build(middlewareQueueName, m, job)
	} else {
		job = NewMiddlewares(mids...).build(middlewareQueueName, m, job)
	}
	m.workers = append(m.workers, newWorker(queue, concurrency, job))
}

func (m *Manager) AddBeforeStartHooks(hooks ...func()) {
	m.lock.Lock()
	defer m.lock.Unlock()
	m.beforeStartHooks = append(m.beforeStartHooks, hooks...)
}

func (m *Manager) AddDuringDrainHooks(hooks ...func()) {
	m.lock.Lock()
	defer m.lock.Unlock()
	m.duringDrainHooks = append(m.duringDrainHooks, hooks...)
}

// Run starts all workers under this Manager and blocks until they exit.
func (m *Manager) Run() {
	m.lock.Lock()
	defer m.lock.Unlock()
	if m.running {
		return // Can't start if we're already running!
	}
	m.running = true

	for _, h := range m.beforeStartHooks {
		h()
	}

	globalStatsServer.registerManager(m)

	var wg sync.WaitGroup

	wg.Add(1)
	m.signal = make(chan os.Signal, 1)
	go func() {
		m.handleSignals()
		wg.Done()
	}()

	wg.Add(len(m.workers))
	for i := range m.workers {
		w := m.workers[i]
		go func() {
			w.start(newSimpleFetcher(w.queue, m.opts))
			wg.Done()
		}()
	}
	m.schedule = newScheduledWorker(m.opts)

	wg.Add(1)
	go func() {
		m.schedule.run()
		wg.Done()
	}()

	// Release the lock so that Stop can acquire it
	m.lock.Unlock()
	wg.Wait()
	// Regain the lock
	m.lock.Lock()
	globalStatsServer.deregisterManager(m)
	m.running = false
}

// Stop all workers under this Manager and returns immediately.
func (m *Manager) Stop() {
	m.lock.Lock()
	defer m.lock.Unlock()
	if !m.running {
		return
	}
	for _, w := range m.workers {
		w.quit()
	}
	m.schedule.quit()
	for _, h := range m.duringDrainHooks {
		h()
	}
	m.stopSignalHandler()
}

func (m *Manager) inProgressMessages() map[string][]*Msg {
	m.lock.Lock()
	defer m.lock.Unlock()
	res := map[string][]*Msg{}
	for _, w := range m.workers {
		res[w.queue] = append(res[w.queue], w.inProgressMessages()...)
	}
	return res
}

func (m *Manager) RetryQueue() string {
	return m.opts.Namespace + retryKey
}

func (m *Manager) Producer() *Producer {
	return &Producer{opts: m.opts}
}
