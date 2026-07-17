package router

func validPreferredScheme(scheme string) bool {
	return scheme == "http" || scheme == "https"
}
