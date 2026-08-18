{ lib, buildGoModule, sourceRevision ? "unknown" }:
buildGoModule {
  pname = "motion-levels-controller";
  version = "unstable";

  src = lib.cleanSourceWith {
    src = ../.;
    filter = path: type:
      let baseName = baseNameOf path;
      in !(baseName == ".git" || baseName == "dist" || baseName == "result");
  };

  vendorHash = null;
  subPackages = [ "cmd/motion-levels-controller" ];
  ldflags = [
    "-s"
    "-w"
    "-X github.com/motionlevels/motion-levels-controller/internal/adapter.BuildRevision=${sourceRevision}"
  ];
  CGO_ENABLED = 0;

  meta = with lib; {
    description = "Motion Levels hardware-facing 16x32 floor controller";
    homepage = "https://github.com/motionlevels/motion-levels-controller";
    mainProgram = "motion-levels-controller";
    platforms = platforms.linux ++ platforms.darwin;
  };
}
