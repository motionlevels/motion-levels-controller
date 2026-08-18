{
  description = "Motion Levels Floor Controller";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
  };

  outputs =
    {
      self,
      nixpkgs,
      ...
    }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = self.packages.${system}.motion-levels-controller;
          motion-levels-controller = pkgs.callPackage ./nix/package.nix {
            sourceRevision = self.rev or (self.dirtyRev or "unknown");
          };
        }
      );

      nixosModules = {
        default = import ./nix/module.nix self;
        motion-levels-floor-controller = self.nixosModules.default;
      };

      overlays.default = final: prev: {
        motion-levels-controller = self.packages.${prev.system}.motion-levels-controller;
      };
    };
}
