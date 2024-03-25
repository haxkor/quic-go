package streamtypebalancer

import (
	"cmp"
	"fmt"
	"sync"

	"github.com/quic-go/quic-go/internal/protocol"
)

type maxNlist[T cmp.Ordered] struct {
	sorted_list []T
	max_elems   int
}

func (ml *maxNlist[T]) add(new_elem T) {
	if len(ml.sorted_list) >= ml.max_elems {
		if new_elem <= ml.sorted_list[ml.max_elems-1] {
			return
		}

		for i, elem := range ml.sorted_list {
			if elem <= new_elem {
				tmp := ml.sorted_list[i : ml.max_elems-1]
				ml.sorted_list = append(ml.sorted_list[:i], new_elem)
				ml.sorted_list = append(ml.sorted_list, tmp...)
			}
		}

	} else {
		ml.sorted_list = append(ml.sorted_list, new_elem)
	}
}

type bitrateHolder struct {
	maxlen  int
	vallist maxNlist[protocol.ByteCount]
	mutex   sync.RWMutex
}

func NewBitrateHolder(maxlen int) *bitrateHolder {
	brh := bitrateHolder{maxlen: maxlen}
	brh.vallist.max_elems = maxlen

	return &brh
}

func (brh *bitrateHolder) Add(elem protocol.ByteCount) {
	brh.mutex.Lock()
	defer brh.mutex.Unlock()

	brh.vallist.add(elem)
}

func (brh *bitrateHolder) shrink(factor float64) {
	brh.mutex.Lock()
	defer brh.mutex.Unlock()

	for i, _ := range brh.vallist.sorted_list {
		elem := float64(brh.vallist.sorted_list[i])
		brh.vallist.sorted_list[i] = protocol.ByteCount(elem * factor)
	}
}

func (brh *bitrateHolder) getMedian() protocol.ByteCount {
	brh.mutex.RLock()
	defer brh.mutex.RUnlock()
	if len(brh.vallist.sorted_list) == 0 {
		return 0
	}

	i := len(brh.vallist.sorted_list) / 2
	return brh.vallist.sorted_list[i]
}

func (brh *bitrateHolder) toString() string {
	brh.mutex.RLock()
	defer brh.mutex.RUnlock()

	result := ""
	for _, elem := range brh.vallist.sorted_list {
		result += fmt.Sprintf("%d, ", elem)
	}
	return result
}
