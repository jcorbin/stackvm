package stackvm

type machAllocator interface {
	AllocMach() (*Mach, error)
	FreeMach(*Mach)
}

type pageAllocator interface {
	AllocPage() *page
	FreePage(*page)
}

func makeMachFreeList(n int) *machFreeList { return &machFreeList{make([]*Mach, 0, n)} }
func makePageFreeList(n int) *pageFreeList { return &pageFreeList{make([]*page, 0, n)} }

type machFreeList struct{ f []*Mach }
type pageFreeList struct{ f []*page }

func (mfl *machFreeList) FreeMach(m *Mach)  { mfl.f = append(mfl.f, m) }
func (pfl *pageFreeList) FreePage(pg *page) { pfl.f = append(pfl.f, pg) }

func (mfl *machFreeList) AllocMach() (*Mach, error) {
	if i := len(mfl.f) - 1; i >= 0 {
		m := mfl.f[i]
		mfl.f = mfl.f[:i]
		return m, nil
	}
	return &Mach{}, nil
}

func (pfl *pageFreeList) AllocPage() *page {
	if i := len(pfl.f) - 1; i >= 0 {
		pg := pfl.f[i]
		pfl.f = pfl.f[:i]
		for i := range pg.d {
			pg.d[i] = 0
		}
		return pg
	}
	return &page{}
}

var defaultMachAllocator machAllocator = _defaultMachAllocator{}
var defaultPageAllocator pageAllocator = _defaultPageAllocator{}

type _defaultMachAllocator struct{}
type _defaultPageAllocator struct{}

func (dmfl _defaultMachAllocator) FreeMach(m *Mach)          {}
func (dpfl _defaultPageAllocator) FreePage(pg *page)         {}
func (dmfl _defaultMachAllocator) AllocMach() (*Mach, error) { return &Mach{}, nil }
func (dpfl _defaultPageAllocator) AllocPage() *page          { return &page{} }