;;; SPDX-License-Identifier: MPL-2.0
;;; No formatting, mounting, device manipulation, tool realization or elevation.
(use-modules (guix secrets) (guix extensions secrets)
             (guix utils)
             (guix-secrets files) (guix-secrets layout) (guix-secrets requests)
             (guix-secrets image) (guix-secrets deploy) (guix-secrets privileged)
             (gnu image) (gnu system) (gnu system file-systems)
             (gnu machine) (gnu machine ssh)
             (gnu bootloader) (gnu bootloader grub)
             (rnrs bytevectors) (rnrs io ports) (srfi srfi-64) (ice-9 ftw))

(define (with-replacements module bindings thunk)
  (let ((old (map (lambda (binding) (cons (car binding) (module-ref module (car binding)))) bindings)))
    (dynamic-wind
      (lambda () (for-each (lambda (binding) (module-set! module (car binding) (cdr binding))) bindings))
      thunk
      (lambda () (for-each (lambda (binding) (module-set! module (car binding) (cdr binding))) old)))))

(test-begin "secrets")
(define runner (test-runner-current))
(define payload '(("/etc/key" #o600 #vu8(0 255 10))))
(define os
  (operating-system
    (host-name "test") (timezone "Etc/UTC") (locale "en_US.utf8")
    (bootloader (bootloader-configuration (bootloader grub-bootloader) (targets '("/dev/vda"))))
    (file-systems (list (file-system (mount-point "/") (device (file-system-label "root")) (type "ext4"))
                        (file-system (mount-point "/var") (device (file-system-label "data")) (type "btrfs")
                                     (options "compress=zstd,subvol=@var"))))))
(define disk
  (image (format 'disk-image) (operating-system os)
         (partitions (list (partition (label "data") (file-system "btrfs"))
                           (partition (label "root") (file-system "ext4") (flags '(boot)))))))
(define extents '((1048576 8388608) (9437184 8388608)))
(test-equal "root is declared, not guessed from filesystem type" 1 (root-partition-index disk))
(test-equal "root geometry follows the declaration"
            `((9437184 8388608 "ext4" () ,payload))
            (provisioning-plan disk extents payload))
(test-equal "child filesystem routing and Btrfs subvolume"
            '((1048576 8388608 "btrfs" ("subvol=@var") (("/lib/key" 384 #vu8(1)))))
            (provisioning-plan disk extents '(("/var/lib/key" 384 #vu8(1)))))
(test-equal "mount options do not leak ordinary boot options"
            "loop,offset=1048576,sizelimit=8388608,nodev,nosuid,noexec,subvol=@var"
            (mount-options 1048576 8388608 '("subvol=@var")))
(test-equal "mount-point boundaries do not capture sibling paths"
            "/" (file-system-mount-point
                  (destination-file-system "/variable/key" (operating-system-file-systems os))))
(test-equal "image root uses Guix's effective filesystem, not host root subvol options"
            `((1048576 8388608 "btrfs" () ,payload))
            (provisioning-plan
             (image (inherit disk)
                    (operating-system
                     (operating-system (inherit os)
                       (file-systems (list (file-system
                                           (mount-point "/") (device "host-root")
                                           (type "btrfs") (options "subvol=@host"))))))
                    (partitions (list (partition (label "root") (file-system "btrfs") (flags '(boot))))))
             '((1048576 8388608)) payload))
(for-each
 (lambda (type)
   (let ((disk (image (inherit disk)
                     (partitions (list (partition (label "root") (file-system type) (flags '(boot))))))))
     (test-equal (string-append "supports " type) type
                 (list-ref (car (provisioning-plan disk '((1048576 8388608)) payload)) 2))))
 '("ext2" "ext3" "ext4" "btrfs"))
(test-error "partition counts must match" #t (provisioning-plan disk '() payload))
(test-error "mount-point itself cannot be a secret file" #t
            (provisioning-plan disk extents '(("/var" 384 #vu8()))))
(test-error "unsupported mounts never fall back to root" #t
            (provisioning-plan
             (image (inherit disk)
                    (operating-system
                     (operating-system (inherit os)
                       (file-systems (cons (file-system (mount-point "/etc") (device "none") (type "tmpfs"))
                                           (operating-system-file-systems os))))))
             extents payload))
(test-error "privileged requests reject mount-option injection" #t
            (validate-plan '((0 100 "btrfs" ("subvol=@,suid") ())) 100))
(test-error "privileged requests reject out-of-bounds extents" #t
            (validate-plan '((100 100 "ext4" () ())) 150))
(test-error "duplicate destinations are files, not repeated writes" #t
            (validate-payload (append payload payload)))
(test-equal "parent/child conflicts are installation errors, not pairwise schema rules"
            '(("/a" 384 #vu8()) ("/a/b" 384 #vu8()))
            (validate-payload '(("/a" 384 #vu8()) ("/a/b" 384 #vu8()))))
(test-assert "SSH uses ordinary commands, no Guile or sudo"
             (let ((command (install-command "/etc/key" #o600 3)))
               (and (equal? "sh" (car command))
                    (not (string-contains (object->string command) "sudo"))
                    (not (string-contains (object->string command) "guile"))
                    (string-contains (list-ref command 2) "mv -fT"))))

;; Fake mounting functions exercise backend selection without running commands.
(let* ((module (resolve-module '(guix-secrets image)))
       (fuse (module-ref module 'call-with-fuse-mount))
       (polkit (module-ref module 'polkit-provision))
       (events '()))
  (dynamic-wind
    (const #t)
    (lambda ()
      (module-set! module 'call-with-fuse-mount (lambda _ (set! events (cons 'fuse events)) #t))
      (module-set! module 'polkit-provision (lambda (_ plan) (set! events (cons plan events))))
      (provision-plan "copy" "mount" "log" '((0 100 "ext3" () ()) (100 100 "btrfs" () ()) (200 100 "ext4" () ())))
      (test-equal "FUSE first, one Polkit request for remaining filesystems"
                  '(fuse ((100 100 "btrfs" () ()) (200 100 "ext4" () ()))) (reverse events))
      (set! events '())
      (module-set! module 'call-with-fuse-mount (lambda _ #f))
      (provision-plan "copy" "mount" "log" '((0 100 "ext2" () ())))
      (test-equal "unavailable FUSE uses Polkit" '(((0 100 "ext2" () ()))) events)
      (set! events '())
      (module-set! module 'call-with-fuse-mount (lambda _ (error "corrupt filesystem")))
      (test-error "filesystem errors propagate" #t
                  (provision-plan "copy" "mount" "log" '((0 100 "ext4" () ()))))
      (test-equal "filesystem errors never request elevation" '() events))
    (lambda () (module-set! module 'call-with-fuse-mount fuse)
               (module-set! module 'polkit-provision polkit))))

;; Ordinary temporary-file I/O only: declaration evaluation and binary contents.
(let* ((dir (mkdtemp "/tmp/guix-secrets-test-XXXXXX"))
       (declaration (string-append dir "/secrets.scm"))
       (source (string-append dir "/source")))
  (dynamic-wind
    (const #t)
    (lambda ()
      (call-with-output-file source (lambda (port) (display "bytes" port)))
      (call-with-output-file declaration
        (lambda (port)
          (display "(use-modules (guix secrets))\n(secrets (files (list (secret-file (source \"source\") (destination \"/etc/key\")) (secret-file (content #vu8(0 255)) (destination \"/etc/binary\")) (secret-file (content \"\") (destination \"/etc/empty\")))))" port)))
      (test-equal "Scheme declarations preserve relative sources, binary and empty payloads"
                  `(("/etc/key" 384 ,(string->utf8 "bytes")) ("/etc/binary" 384 #vu8(0 255)) ("/etc/empty" 384 #vu8()))
                  (load-secrets declaration))
      (call-with-output-file declaration
        (lambda (port) (display "(error \"PRIVATE-ERROR-MARKER\")" port)))
      (test-error "private evaluation errors propagate without load* reporting" #t (load-secrets declaration)))
    (lambda () (delete-file source) (delete-file declaration) (rmdir dir))))

;; Native image-type selection and direct image declarations, without building.
(let* ((name (string-copy "/tmp/guix-secrets-spec-XXXXXX"))
       (port (mkstemp! name))
       (private (string-copy "/tmp/guix-secrets-cli-XXXXXX"))
       (private-port (mkstemp! private))
       (definition "(use-modules (gnu system) (gnu image) (gnu bootloader) (gnu bootloader grub) (gnu system file-systems))\n(define os (operating-system (host-name \"test\") (timezone \"Etc/UTC\") (locale \"en_US.utf8\") (bootloader (bootloader-configuration (bootloader grub-bootloader) (targets '(\"/dev/vda\")))) (file-systems (list (file-system (mount-point \"/\") (device \"/dev/vda1\") (type \"ext4\"))))))\n"))
  (close-port port)
  (display "(secrets)\n" private-port) (close-port private-port)
  (dynamic-wind
    (const #t)
    (lambda ()
      (call-with-output-file name (lambda (port) (display definition port) (display "os\n" port)))
      (test-equal "OS uses native default image type" 'mbr
                  (image-partition-table-type (resolve-image name #f)))
      (test-equal "OS accepts native image-type selection" 'gpt
                  (image-partition-table-type (resolve-image name "efi-raw")))
      (call-with-output-file name
        (lambda (port)
          (display definition port)
          (display "(image (format 'disk-image) (operating-system os) (partitions (list (partition (label \"explicit\") (file-system \"btrfs\") (flags '(boot))))))\n" port)))
      (test-equal "explicit image supplies its own layout" "explicit"
                  (partition-label (car (image-partitions (resolve-image name "efi-raw")))))
      (let ((native (%current-system)) (target (%current-target-system)) (observed #f))
        (with-replacements
         (resolve-module '(guix extensions secrets))
         (list (cons 'provision-image
                     (lambda* (input output files declaration #:key evaluate)
                       (set! observed
                             (list (%current-system) (%current-target-system)
                                   (evaluate (lambda () (list (%current-system) (%current-target-system)))))))))
         (lambda ()
           (guix-secrets "image" "--system=aarch64-linux" (string-append "--secrets=" private)
                         "--artifact=unused" "--output=unused" name)
           (test-equal "target-system evaluation does not change runner tool architecture"
                       (list native target (list "aarch64-linux" target)) observed)
           (guix-secrets "image" "--target=aarch64-linux-gnu" (string-append "--secrets=" private)
                         "--artifact=unused" "--output=unused" name)
           (test-equal "cross target applies only to declaration evaluation"
                       (list native target (list native "aarch64-linux-gnu")) observed)))))
    (lambda () (delete-file private) (delete-file name))))

;; The privileged backend is exercised with fake commands, including cleanup
;; failure. No mount/device operation or real privilege change is performed.
(let* ((name (string-copy "/tmp/guix-secrets-helper-test-XXXXXX"))
       (port (mkstemp! name)) (events '()) (fail-install? #f) (fail-unmount? #f)
       (retained #f)
       (plan '((0 100 "ext3" () ()) (100 100 "btrfs" ("subvol=@var") ()))))
  (truncate-file port 200) (close-port port)
  (dynamic-wind
    (const #t)
    (lambda ()
      (with-replacements
       (resolve-module '(guix-secrets privileged))
       (list (cons 'getuid (lambda () 0))
             (cons 'system*
                   (lambda (program . args)
                     (set! events (cons (cons program args) events))
                     (if (and fail-unmount? (string=? program "umount"))
                         (begin (set! retained (cadr args)) 1) 0)))
             (cons 'install-payload
                   (lambda _ (when fail-install? (error "copy failed")))))
       (lambda ()
         (provision-with-mount name plan "mount" "umount")
         (test-equal "kernel mounts use requested ext/Btrfs types and unmount each"
                     '("mount" "umount" "mount" "umount") (map car (reverse events)))
         (test-equal "Btrfs subvolume is passed to the kernel mount"
                     "loop,offset=100,sizelimit=100,nodev,nosuid,noexec,subvol=@var"
                     (list-ref (list-ref (reverse events) 2) 4))
         (set! events '()) (set! fail-install? #t)
         (test-error "copy failure propagates from privileged backend" #t
                     (provision-with-mount name plan "mount" "umount"))
         (test-equal "copy failure still unmounts before returning"
                     '("mount" "umount") (map car (reverse events)))
         (set! fail-install? #f) (set! fail-unmount? #t)
         (parameterize ((current-error-port (open-output-string)))
           (test-error "unmount failure prevents safe cleanup" 'mount-cleanup-failed
                       (provision-with-mount name plan "mount" "umount")))
         (test-assert "failed unmount retains the mount directory" (file-exists? retained)))))
    (lambda () (when retained (rmdir retained)) (delete-file name))))

;; No SSH server or remote shell: exercise binary transfer, machine session use,
;; error propagation and disconnect through replaceable transport procedures.
(let* ((name (string-copy "/tmp/guix-secrets-ssh-XXXXXX"))
       (port (mkstemp! name))
       (machine (machine (operating-system os) (environment managed-host-environment-type)
                         (configuration (machine-ssh-configuration
                                         (host-name "test") (system "x86_64-linux")
                                         (user "admin") (session 'test-session)))))
       (captures '()) (commands '()) (disconnected 0) (status 0))
  (close-port port)
  (dynamic-wind
    (const #t)
    (lambda ()
      (with-replacements
       (resolve-module '(guix-secrets deploy))
       (list (cons 'open-remote-pipe*
                   (lambda (session mode . command)
                     (unless (eq? session 'test-session) (error "machine session ignored"))
                     (set! commands (cons command commands))
                     (open-file name "w+")))
             (cons 'channel-send-eof
                   (lambda (pipe) (seek pipe 0 SEEK_SET)
                     (let ((bytes (get-bytevector-all pipe)))
                       (set! captures (cons (if (eof-object? bytes) #vu8() bytes) captures)))))
             (cons 'channel-get-exit-status (lambda (_) status))
             (cons 'disconnect! (lambda (_) (set! disconnected (+ 1 disconnected)))))
       (lambda ()
         (deploy-files '(("/etc/key" 384 #vu8(0 255 10)) ("/etc/鍵" 416 #vu8())) (list machine machine))
         (test-equal "all machines receive binary/empty payloads unchanged"
                     '(#vu8(0 255 10) #vu8() #vu8(0 255 10) #vu8()) (reverse captures))
         (test-equal "each machine session is released" 2 disconnected)
         (test-equal "SSH includes expected byte count, mode, Unicode path"
                     '("/etc/鍵" "640" "0") (list-tail (car commands) 4))
         (set! status 1)
         (test-error "copy permission errors are propagated" #t (deploy-files payload (list machine)))
         (test-equal "failed transfer still releases session" 3 disconnected))))
    (lambda () (delete-file name))))

;; Real parser, fake inspection commands. Only a sparse ordinary file is created.
(let* ((name (string-copy "/tmp/guix-secrets-geometry-XXXXXX"))
       (port (mkstemp! name))
       (module (resolve-module '(guix-secrets image)))
       (root-label "root"))
  (truncate-file port 17825792) (close-port port)
  (dynamic-wind
    (const #t)
    (lambda ()
      (with-replacements
       module
       (list (cons 'tool (lambda (package path) path))
             (cons 'command-output
                   (lambda (program . args)
                     (if (string-suffix? "sfdisk" program)
                         "{\"partitiontable\":{\"label\":\"dos\",\"unit\":\"sectors\",\"sectorsize\":512,\"partitions\":[{\"start\":2048,\"size\":16384},{\"start\":18432,\"size\":16384}]}}"
                         (if (member "1048576" args) "TYPE=btrfs\nLABEL=data\n"
                             (string-append "TYPE=ext4\nLABEL=" root-label "\n"))))))
       (lambda ()
         (test-equal "geometry is read from artifact, not declaration size guesses"
                     extents (image-extents name disk))
         (set! root-label "wrong")
         (test-error "artifact identity mismatch is rejected before mounting" #t (image-extents name disk)))))
    (lambda () (delete-file name))))
(test-end "secrets")
(exit (if (zero? (test-runner-fail-count runner)) 0 1))
