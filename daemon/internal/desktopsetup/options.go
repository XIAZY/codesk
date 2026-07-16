package desktopsetup

type Options struct {
	Version   string
	Arch      string
	Quiet     bool
	NoLaunch  bool
	Uninstall bool
}

func validateOptions(options Options) error {
	if err := validateVersion(options.Version); err != nil {
		return err
	}
	if err := validateArch(options.Arch); err != nil {
		return err
	}
	return nil
}
