package runtime

import (
	"crypto/rand"
	"math/big"
)

var (
	adjectives = []string{
		"brave", "calm", "clever", "eager", "fancy", "gentle", "happy", "jolly", "keen", "lucid",
		"merry", "nimble", "proud", "quiet", "rapid", "sharp", "sleepy", "steady", "swift", "witty",
	}
	nouns = []string{
		"avie", "bsd", "darwin", "dyld", "hfs", "kext", "launchd", "mach", "macho", "nub",
		"panther", "puma", "tiger", "leopard", "lion", "jaguar", "cheetah", "xnu", "zone", "iokit",
	}
)

func pick(list []string) string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(list))))
	if err != nil {
		panic(err)
	}
	return list[n.Int64()]
}

func generateName() string {
	return pick(adjectives) + "_" + pick(nouns) + "_" + randomHex(2)
}
