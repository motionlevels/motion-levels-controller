self:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.motion-levels-floor-controller;
  types = lib.types;
in
{
  options.services.motion-levels-floor-controller = {
    enable = lib.mkEnableOption "Motion Levels floor controller service";

    package = lib.mkOption {
      type = types.package;
      default =
        if self ? packages.${pkgs.system}.default then
          self.packages.${pkgs.system}.default
        else
          pkgs.callPackage ./package.nix { };
      defaultText = lib.literalExpression "pkgs.motion-levels-controller";
      description = "The motion-levels-controller package to run.";
    };

    httpAddr = lib.mkOption {
      type = types.str;
      default = "127.0.0.1:4200";
      description = "Loopback HTTP address for health and metrics.";
    };

    engineAddr = lib.mkOption {
      type = types.str;
      default = "127.0.0.1:4201";
      description = "Loopback TCP address for the single engine stream.";
    };

    recvPort = lib.mkOption {
      type = types.port;
      default = 7800;
      description = "UDP port for floor handshake and sensor packets.";
    };

    floorSourceIP = lib.mkOption {
      type = types.nullOr types.str;
      default = null;
      description = "Exact local IPv4 source for floor UDP output; null uses default route.";
    };

    floorRotation = lib.mkOption {
      type = types.enum [
        0
        180
      ];
      default = 0;
      description = "Logical-to-physical floor rotation in degrees (0 or 180).";
    };

    broadcastIP = lib.mkOption {
      type = types.str;
      default = "255.255.255.255";
      description = "Floor LED broadcast IPv4 address.";
    };

    watchdogSec = lib.mkOption {
      type = types.nullOr types.str;
      default = "10s";
      description = "Systemd watchdog timeout (null to disable).";
    };

    extraArgs = lib.mkOption {
      type = types.listOf types.str;
      default = [ ];
      description = "Extra CLI arguments to pass to motion-levels-controller.";
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.services.motion-levels-floor-controller = {
      description = "Motion Levels floor controller";
      after = [ "network.target" ];
      wantedBy = [ "multi-user.target" ];

      serviceConfig = {
        Type = "notify";
        NotifyAccess = "main";
        WatchdogSec = cfg.watchdogSec;
        ExecStart = lib.escapeShellArgs (
          [
            "${cfg.package}/bin/motion-levels-controller"
            "-http"
            cfg.httpAddr
            "-engine"
            cfg.engineAddr
            "-recv-port"
            (toString cfg.recvPort)
            "-floor-rotation"
            (toString cfg.floorRotation)
            "-broadcast-ip"
            cfg.broadcastIP
          ]
          ++ lib.optionals (cfg.floorSourceIP != null) [
            "-floor-source-ip"
            cfg.floorSourceIP
          ]
          ++ cfg.extraArgs
        );
        Restart = "always";
        RestartSec = "2s";

        # Security hardening
        ProtectSystem = "strict";
        ProtectHome = true;
        NoNewPrivileges = true;
      };
    };
  };
}
