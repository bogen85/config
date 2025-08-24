// repeatable flags: --volume VAL --volume VAL ...
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	if v == "" {
		return fmt.Errorf("empty value not allowed")
	}
	*m = append(*m, v)
	return nil
}
