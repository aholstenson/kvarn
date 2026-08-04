package mirror_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/scm/mirror"
)

var _ = Describe("Store", func() {
	var (
		ctx     context.Context
		tmpDir  string
		storeIn string
		bareDir string
		workDir string
		store   *mirror.Store
		ref     mirror.Ref
	)

	BeforeEach(func() {
		ctx = context.Background()
		tmpDir = GinkgoT().TempDir()
		storeIn = filepath.Join(tmpDir, "repos")
		bareDir = filepath.Join(tmpDir, "upstream.git")
		workDir = filepath.Join(tmpDir, "work")

		runOK("git", "init", "--bare", "--initial-branch=main", bareDir)
		runOK("git", "clone", bareDir, workDir)
		commit(workDir, "a.txt", "a\n", "first")
		runIn(workDir, "git", "push", "origin", "HEAD:main")

		store = mirror.New(storeIn)
		ref = mirror.Ref{Project: "demo", URL: bareDir}
	})

	Describe("Refresh", func() {
		It("populates a cold mirror with the requested branch", func() {
			Expect(store.Refresh(ctx, ref, "main")).To(Succeed())

			entries, err := store.List()
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Project).To(Equal("demo"))
			Expect(entries[0].URL).To(Equal(bareDir))
			Expect(entries[0].Branches).To(ConsistOf("main"))
			Expect(entries[0].SizeBytes).To(BeNumerically(">", 0))
		})

		It("acquires a second branch as a delta on the shared objects", func() {
			Expect(store.Refresh(ctx, ref, "main")).To(Succeed())
			sizeAfterFirst := sizeOf(store)

			runIn(workDir, "git", "checkout", "-b", "feature")
			commit(workDir, "b.txt", "b\n", "second")
			runIn(workDir, "git", "push", "origin", "feature")

			Expect(store.Refresh(ctx, ref, "feature")).To(Succeed())

			entries, err := store.List()
			Expect(err).NotTo(HaveOccurred())
			Expect(entries[0].Branches).To(ConsistOf("main", "feature"))
			// A second branch on shared history must not cost a second copy of
			// the repository.
			Expect(sizeOf(store)).To(BeNumerically("<", sizeAfterFirst*2))
		})

		It("skips the fetch when the branch has not moved", func() {
			Expect(store.Refresh(ctx, ref, "main")).To(Succeed())
			before := lastFetch(store, "demo", "main")

			// Make the upstream unreachable. A refresh that still succeeds
			// proves the ls-remote comparison short-circuited the fetch... but
			// ls-remote itself needs the upstream, so instead assert the
			// recorded fetch time did not move.
			time.Sleep(10 * time.Millisecond)
			Expect(store.Refresh(ctx, ref, "main")).To(Succeed())
			Expect(lastFetch(store, "demo", "main")).To(Equal(before))
		})

		It("touches nothing at all when the wanted commit is already mirrored", func() {
			Expect(store.Refresh(ctx, ref, "main")).To(Succeed())
			sha := strings.TrimSpace(outputIn(workDir, "git", "rev-parse", "HEAD"))

			// With the upstream gone, a refresh can only succeed if it never
			// reaches for it — which is exactly the feedback-run fast path.
			Expect(os.RemoveAll(bareDir)).To(Succeed())
			Expect(store.Clone(ctx, mirror.CloneOpts{
				Ref:         ref,
				Branch:      "main",
				Destination: filepath.Join(tmpDir, "warm"),
				WantSHA:     sha,
			})).To(Succeed())
		})

		It("fetches a branch that moved upstream", func() {
			Expect(store.Refresh(ctx, ref, "main")).To(Succeed())

			commit(workDir, "c.txt", "c\n", "third")
			runIn(workDir, "git", "push", "origin", "HEAD:main")
			want := strings.TrimSpace(outputIn(workDir, "git", "rev-parse", "HEAD"))

			Expect(store.Refresh(ctx, ref, "main")).To(Succeed())
			Expect(mirrorRef(storeIn, "demo", "main")).To(Equal(want))
		})

		It("rebuilds a mirror whose object store has been destroyed", func() {
			Expect(store.Refresh(ctx, ref, "main")).To(Succeed())
			Expect(os.RemoveAll(filepath.Join(storeIn, "demo", "mirror.git", "objects"))).To(Succeed())

			Expect(store.Refresh(ctx, ref, "main")).To(Succeed())
			Expect(mirrorRef(storeIn, "demo", "main")).NotTo(BeEmpty())
		})

		It("treats a flag-shaped branch as a ref", func() {
			err := store.Refresh(ctx, ref, "--upload-pack=false")
			Expect(err).To(HaveOccurred())
			// Not found as a branch, rather than acted on as an option.
			Expect(err.Error()).To(ContainSubstring("not found"))
		})
	})

	Describe("Clone", func() {
		It("produces a worktree with no remote", func() {
			dest := filepath.Join(tmpDir, "job")
			Expect(store.Clone(ctx, mirror.CloneOpts{
				Ref: ref, Branch: "main", Destination: dest,
			})).To(Succeed())

			Expect(filepath.Join(dest, "a.txt")).To(BeAnExistingFile())
			Expect(strings.TrimSpace(outputIn(dest, "git", "remote"))).To(BeEmpty())
			// The mirror path must not survive anywhere in the job clone.
			config, err := os.ReadFile(filepath.Join(dest, ".git", "config"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(config)).NotTo(ContainSubstring(storeIn))
		})

		It("honours a shallow depth", func() {
			commit(workDir, "d.txt", "d\n", "second")
			runIn(workDir, "git", "push", "origin", "HEAD:main")

			dest := filepath.Join(tmpDir, "shallow")
			Expect(store.Clone(ctx, mirror.CloneOpts{
				Ref: ref, Branch: "main", Destination: dest, Depth: 1,
			})).To(Succeed())

			// The regression guard for the local-clone gotcha: a plain path
			// would have hardlinked the whole history and ignored Depth.
			Expect(filepath.Join(dest, ".git", "shallow")).To(BeAnExistingFile())
			Expect(strings.TrimSpace(outputIn(dest, "git", "rev-list", "--count", "HEAD"))).To(Equal("1"))
		})

		It("serves concurrent clones from one mirror", func() {
			var wg sync.WaitGroup
			errs := make([]error, 8)
			for i := range errs {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					defer GinkgoRecover()
					errs[i] = store.Clone(ctx, mirror.CloneOpts{
						Ref:         ref,
						Branch:      "main",
						Destination: filepath.Join(tmpDir, "concurrent", "c"+string(rune('a'+i))),
					})
				}(i)
			}
			wg.Wait()
			for _, err := range errs {
				Expect(err).NotTo(HaveOccurred())
			}
		})
	})

	Describe("Prune and GC", func() {
		It("drops branch refs no job has asked for recently", func() {
			runIn(workDir, "git", "checkout", "-b", "stale")
			commit(workDir, "s.txt", "s\n", "stale")
			runIn(workDir, "git", "push", "origin", "stale")

			Expect(store.Refresh(ctx, ref, "main")).To(Succeed())
			Expect(store.Refresh(ctx, ref, "stale")).To(Succeed())

			// A retention shorter than every recorded use prunes both; a very
			// long one keeps them.
			Expect(store.Prune(ctx, time.Hour)).To(Succeed())
			entries, err := store.List()
			Expect(err).NotTo(HaveOccurred())
			Expect(entries[0].Branches).To(ConsistOf("main", "stale"))

			Expect(store.Prune(ctx, time.Nanosecond)).To(Succeed())
			entries, err = store.List()
			Expect(err).NotTo(HaveOccurred())
			Expect(entries[0].Branches).To(BeEmpty())
			Expect(mirrorRef(storeIn, "demo", "stale")).To(BeEmpty())
		})

		It("repacks without breaking the mirror", func() {
			Expect(store.Refresh(ctx, ref, "main")).To(Succeed())
			Expect(store.GC(ctx, "demo")).To(Succeed())

			dest := filepath.Join(tmpDir, "after-gc")
			Expect(store.Clone(ctx, mirror.CloneOpts{
				Ref: ref, Branch: "main", Destination: dest,
			})).To(Succeed())
			Expect(filepath.Join(dest, "a.txt")).To(BeAnExistingFile())
		})
	})

	Describe("Evict and Remove", func() {
		It("removes a mirror entirely", func() {
			Expect(store.Refresh(ctx, ref, "main")).To(Succeed())
			Expect(store.Remove(ctx, "demo")).To(Succeed())

			entries, err := store.List()
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())
		})

		It("evicts down to a global limit, least recently used first", func() {
			Expect(store.Refresh(ctx, ref, "main")).To(Succeed())
			// A limit of one byte cannot be met by anything, so the sweep
			// removes every mirror.
			Expect(store.Evict(ctx, 1)).To(Succeed())

			entries, err := store.List()
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())
		})

		It("keeps everything when no limit is set", func() {
			Expect(store.Refresh(ctx, ref, "main")).To(Succeed())
			Expect(store.Evict(ctx, 0)).To(Succeed())

			entries, err := store.List()
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
		})
	})
})

func sizeOf(s *mirror.Store) int64 {
	GinkgoHelper()
	entries, err := s.List()
	Expect(err).NotTo(HaveOccurred())
	var total int64
	for _, e := range entries {
		total += e.SizeBytes
	}
	return total
}

func lastFetch(s *mirror.Store, project, branch string) time.Time {
	GinkgoHelper()
	entries, err := s.List()
	Expect(err).NotTo(HaveOccurred())
	for _, e := range entries {
		if e.Project == project {
			return e.LastFetch
		}
	}
	Fail("no mirror for " + project)
	return time.Time{}
}

func mirrorRef(storeDir, project, branch string) string {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = filepath.Join(storeDir, project, "mirror.git")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func commit(dir, name, content, message string) {
	GinkgoHelper()
	Expect(os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)).To(Succeed())
	runIn(dir, "git", "add", name)
	runIn(dir, "git", "-c", "user.email=t@t", "-c", "user.name=T", "commit", "-m", message)
}

func runOK(name string, args ...string) {
	GinkgoHelper()
	out, err := exec.Command(name, args...).CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), string(out))
}

func runIn(dir string, name string, args ...string) {
	GinkgoHelper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), string(out))
}

func outputIn(dir string, name string, args ...string) string {
	GinkgoHelper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	Expect(err).NotTo(HaveOccurred())
	return string(out)
}
