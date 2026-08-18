{
  description = "Motion Levels Floor Controller";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs = { self, nixpkgs, ... }:
    let
      lib = nixpkgs.lib;
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = lib.genAttrs systems;
    in {
      packages = forAllSystems (system:
        let pkgs = nixpkgs.legacyPackages.${system};
        in {
          default = self.packages.${system}.motion-levels-controller;
          motion-levels-controller = pkgs.callPackage ./nix/package.nix {
            sourceRevision = self.rev or (self.dirtyRev or "unknown");
          };
        });

      checks = forAllSystems (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          package = self.packages.${system}.default;
          moduleSystem = lib.nixosSystem {
            inherit system;
            modules = [
              self.nixosModules.default
              {
                system.stateVersion = "26.05";
                services.motion-levels-floor-controller = {
                  enable = true;
                  inherit package;
                  floorSourceIP = "192.0.2.10";
                };
              }
            ];
          };
          service = moduleSystem.config.systemd.services.motion-levels-floor-controller;
          addressFamilies = lib.concatStringsSep " " service.serviceConfig.RestrictAddressFamilies;
        in {
          inherit package;
        } // lib.optionalAttrs pkgs.stdenv.isLinux {
          nixos-module = pkgs.runCommand "motion-levels-controller-nixos-module-check" { } ''
            test ${lib.escapeShellArg service.serviceConfig.Type} = notify
            test -n ${lib.escapeShellArg service.serviceConfig.ExecStart}
            case ${lib.escapeShellArg addressFamilies} in
              *AF_NETLINK*) ;;
              *) echo "NixOS sandbox must permit AF_NETLINK for net.Interfaces" >&2; exit 1 ;;
            esac
            touch "$out"
          '';
        });

      nixosModules = {
        default = import ./nix/module.nix self;
        motion-levels-floor-controller = self.nixosModules.default;
      };

      overlays.default = final: prev: {
        motion-levels-controller = self.packages.${prev.system}.motion-levels-controller;
      };
    };
}
