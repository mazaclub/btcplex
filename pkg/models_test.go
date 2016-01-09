package btcplex

import (
	"testing"
)

func TestGetBlockReward(t *testing.T) {
	type blockRewardTest struct {
		Height uint
		Reward uint64
	}

	rewardTests := []blockRewardTest{
		{100, 5000 * COIN},
		{100000, 1000 * COIN},
	}

	for _, item := range rewardTests {
		blockReward := GetBlockReward(item.Height)
		if blockReward != item.Reward {
			t.Error("for block reward of", item.Height,
				"expected", item.Reward, "got", blockReward)
		}
	}
}
