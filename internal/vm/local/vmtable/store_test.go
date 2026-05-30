package vmtable_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/vm/local/vmtable"
)

var _ = Describe("Store", func() {
	var path string

	BeforeEach(func() {
		path = filepath.Join(GinkgoT().TempDir(), "vms.json")
	})

	It("returns an empty store when the file does not exist", func() {
		s, err := vmtable.Open(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(s.List()).To(BeEmpty())
	})

	It("round-trips entries through Add / Open / List", func() {
		s, err := vmtable.Open(path)
		Expect(err).NotTo(HaveOccurred())

		e := vmtable.Entry{
			ID:      "local-1",
			PID:     1234,
			CID:     5,
			Comm:    "qemu-system-x86_64",
			TmpDisk: "/tmp/kvarn-disk-x.qcow2",
		}
		Expect(s.Add(e)).To(Succeed())

		reopened, err := vmtable.Open(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(reopened.List()).To(ConsistOf(e))
	})

	It("replaces an entry with the same ID instead of duplicating it", func() {
		s, err := vmtable.Open(path)
		Expect(err).NotTo(HaveOccurred())

		Expect(s.Add(vmtable.Entry{ID: "vm", PID: 1})).To(Succeed())
		Expect(s.Add(vmtable.Entry{ID: "vm", PID: 2})).To(Succeed())

		entries := s.List()
		Expect(entries).To(HaveLen(1))
		Expect(entries[0].PID).To(Equal(2))
	})

	It("removes entries by ID", func() {
		s, err := vmtable.Open(path)
		Expect(err).NotTo(HaveOccurred())

		Expect(s.Add(vmtable.Entry{ID: "a"})).To(Succeed())
		Expect(s.Add(vmtable.Entry{ID: "b"})).To(Succeed())
		Expect(s.Remove("a")).To(Succeed())

		ids := []string{}
		for _, e := range s.List() {
			ids = append(ids, e.ID)
		}
		Expect(ids).To(Equal([]string{"b"}))
	})

	It("treats Remove of a missing id as a no-op", func() {
		s, err := vmtable.Open(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(s.Remove("missing")).To(Succeed())
	})

	It("never produces a torn file under a concurrent reader loop", func() {
		s, err := vmtable.Open(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(s.Add(vmtable.Entry{ID: "warm"})).To(Succeed())

		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				data, err := os.ReadFile(path)
				if err != nil {
					if os.IsNotExist(err) {
						continue
					}
					Fail(err.Error())
				}
				if len(data) == 0 {
					continue
				}
				var got []vmtable.Entry
				Expect(json.Unmarshal(data, &got)).To(Succeed())
			}
		}()

		for i := 0; i < 100; i++ {
			Expect(s.Add(vmtable.Entry{ID: "vm", PID: i})).To(Succeed())
		}
		close(stop)
		wg.Wait()
	})

	It("DefaultPath returns a stable absolute path", func() {
		Expect(filepath.IsAbs(vmtable.DefaultPath())).To(BeTrue())
	})
})
