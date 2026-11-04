# Repository entry point. SDK targets live with the core they build.
.DEFAULT_GOAL := all
CORE_GOALS := $(if $(MAKECMDGOALS),$(MAKECMDGOALS),all)

.PHONY: $(CORE_GOALS)
$(firstword $(CORE_GOALS)):
	+$(MAKE) -C core $(CORE_GOALS)

ifneq ($(word 2,$(CORE_GOALS)),)
$(wordlist 2,$(words $(CORE_GOALS)),$(CORE_GOALS)): $(firstword $(CORE_GOALS))
	@:
endif
