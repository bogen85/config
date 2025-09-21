package util

type errAtoi struct{}

func (errAtoi) Error() string { return "not a number" }

func AtoiSafe(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errAtoi{}
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}
