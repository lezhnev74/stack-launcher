package httpserver

import (
	"context"
	"crypto/rand"
	"errors"
	"strconv"
	"strings"
	"time"

	"stack-launcher.local/app/internal/caddy"
	"stack-launcher.local/app/internal/ledger"
)

const stackNameLimit = 32
const plainNameAttempts = 16

var errBaseDomainLength = errors.New("base domain leaves no room for stack ID")

var stackAdverbs = strings.Fields(`
aptly boldly briskly calmly evenly fairly firmly fondly
freely gladly gently gravely greatly highly honestly idly
keenly kindly lately lightly likely limply loudly loyally
mainly merrily merely neatly nicely nobly openly plainly
politely poorly primly proudly purely quickly quietly rarely
richly rightly roughly safely sharply simply slowly slyly
smugly softly solely soundly squarely sternly surely swiftly
tamely tenderly tightly truly vastly warmly weakly wisely
`)

var stackNouns = strings.Fields(`
acorn antelope apple badger bamboo beaver berry birch
bison blossom brook canyon cedar cherry cloud clover
comet coral crane creek daisy delta dune eagle
elm falcon fern finch fjord forest fox frost
garden gecko grove hare hazel heron hill iris
island ivy lake lark laurel maple meadow mint
moon moss oak oasis otter owl pebble pine
rain raven reed ridge robin seal star willow
`)

func stackName(date time.Time, index int) string {
	return stackAdverbs[index/len(stackNouns)] + "-" + stackNouns[index%len(stackNouns)] + "-" + date.UTC().Format("20060102")
}

func (a *api) reserveLaunch(ctx context.Context, now time.Time, duration time.Duration, env ledger.Environment, template string, request *ledger.LaunchRequest) (string, string, error) {
	if !a.validLaunchDomain() {
		return "", "", errBaseDomainLength
	}
	id, err := reserveStackName(now, func(id string) error {
		return a.claimLaunch(ctx, id, id+"."+a.cfg.BaseDomain, duration, env, template, request)
	})
	return id, id + "." + a.cfg.BaseDomain, err
}

func (a *api) validLaunchDomain() bool {
	return caddy.ValidDomain(strings.Repeat("a", stackNameLimit) + "." + a.cfg.BaseDomain)
}

func reserveStackName(now time.Time, claim func(string) error) (string, error) {
	var random [2]byte
	_, _ = rand.Read(random[:]) // The default crypto/rand reader always succeeds or terminates the process.
	count := len(stackAdverbs) * len(stackNouns)
	first := (int(random[0])<<8 | int(random[1])) % count
	for attempt := 0; ; attempt++ {
		id := stackName(now, (first+attempt)%count)
		if attempt >= plainNameAttempts {
			// Keep trying new word pairs and add a suffix only when the plain names collide.
			id += "-" + strconv.Itoa(2+(attempt-plainNameAttempts)/count)
		}
		if len(id) > stackNameLimit {
			return "", errors.New("all stack names for this date are in use")
		}
		err := claim(id)
		if !errors.Is(err, ledger.ErrStackNameTaken) {
			return id, err
		}
	}
}

func (a *api) claimLaunch(ctx context.Context, id, host string, duration time.Duration, env ledger.Environment, template string, request *ledger.LaunchRequest) error {
	if _, loaded := a.operations.LoadOrStore(id, struct{}{}); loaded {
		return ledger.ErrStackNameTaken
	}
	err := a.db.CreateLaunch(ctx, id, host, duration, env, template, request)
	if err != nil {
		a.operations.Delete(id)
	}
	return err
}
