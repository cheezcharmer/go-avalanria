Pod::Spec.new do |spec|
  spec.name         = 'Gavn'
  spec.version      = '{{.Version}}'
  spec.license      = { :type => 'GNU Lesser General Public License, Version 3.0' }
  spec.homepage     = 'https://github.com/avalanria/go-avalanria'
  spec.authors      = { {{range .Contributors}}
		'{{.Name}}' => '{{.Email}}',{{end}}
	}
  spec.summary      = 'iOS Avalanria Client'
  spec.source       = { :git => 'https://github.com/avalanria/go-avalanria.git', :commit => '{{.Commit}}' }

	spec.platform = :ios
  spec.ios.deployment_target  = '9.0'
	spec.ios.vendored_frameworks = 'Frameworks/Gavn.framework'

	spec.prepare_command = <<-CMD
    curl https://gavnstore.blob.core.windows.net/builds/{{.Archive}}.tar.gz | tar -xvz
    mkdir Frameworks
    mv {{.Archive}}/Gavn.framework Frameworks
    rm -rf {{.Archive}}
  CMD
end
