/* Crystalline glaze study: procedural colour pooling, feathered crystal blooms
 * and fine mineral deposits. Uses the shared orb's audio-driven flow in a disk.
 * An art-directed moving material, not a simulation of ceramic firing. */
(() => {
  'use strict';
  window.MeadowInkMaterial = {
    palettes: {
      meadow: {label:'青藍釉',swatch:'#548b96',colors:[[.86,.88,.69],[.53,.75,.68],[.19,.43,.52],[.045,.13,.27]]},
      cobalt: {label:'鈷紫釉',swatch:'#34317b',colors:[[.77,.64,.83],[.62,.28,.60],[.13,.13,.54],[.025,.025,.13]]},
      jade: {label:'翡翠釉',swatch:'#789982',colors:[[.89,.85,.53],[.52,.76,.66],[.33,.45,.27],[.12,.24,.19]]},
      iron: {label:'鐵黑釉',swatch:'#424843',colors:[[.71,.74,.65],[.44,.49,.46],[.23,.25,.23],[.065,.075,.07]]},
      bronze: {label:'鐵褐釉',swatch:'#79644c',colors:[[.79,.81,.68],[.55,.61,.52],[.38,.28,.18],[.105,.085,.06]]},
      frost: {label:'霜藍釉',swatch:'#5a7cae',colors:[[.84,.89,.79],[.56,.72,.89],[.20,.33,.61],[.10,.055,.19]]},
    },
    fragment: `
      precision mediump float;
      varying vec2 v_uv;
      uniform vec2 u_resolution;
      uniform float u_time, u_energy;
      uniform vec4 u_flow;
      uniform vec3 u_air, u_mist, u_green, u_forest;
      float hash(vec2 p) {
        vec3 q=fract(vec3(p.xyx)*.1031);
        q+=dot(q,q.yzx+33.33);
        return fract((q.x+q.y)*q.z);
      }
      float noise(vec2 p) {
        vec2 i=floor(p),f=fract(p);f=f*f*(3.0-2.0*f);
        return mix(mix(hash(i),hash(i+vec2(1,0)),f.x),mix(hash(i+vec2(0,1)),hash(i+vec2(1)),f.x),f.y);
      }
      float fbm(vec2 p) {
        float sum=0.0,amp=.5;
        for(int i=0;i<4;i++){sum+=noise(p)*amp;p=mat2(.8,-.6,.6,.8)*p*2.07+3.7;amp*=.5;}
        return sum;
      }
      // Irregular rosettes, with radial feathering and a mineral-rich edge.
      // Cells move in material space, so crystals travel with the glaze.
      vec2 crystals(vec2 p) {
        vec2 cell=floor(p),f=fract(p);
        float bloom=0.0,facets=0.0;
        for(int y=-1;y<=1;y++) for(int x=-1;x<=1;x++) {
          vec2 g=vec2(float(x),float(y));
          float seed=hash(cell+g);
          vec2 centre=.2+.6*vec2(seed,hash(cell+g+29.7));
          vec2 d=f-g-centre;
          float angle=atan(d.y,d.x),r=length(d);
          float petals=6.0+floor(seed*5.0);
          float radius=(.18+.23*hash(cell+g+13.1))*(1.0+.19*cos(angle*petals+seed*12.0));
          float edge=radius-r;
          float silhouette=smoothstep(-.035,.035,edge);
          float feather=.53+.47*pow(abs(cos(angle*petals*2.0+sin(r*65.0)*.35)),6.0);
          float rings=.68+.32*sin(r*108.0+seed*8.0);
          float presence=smoothstep(.18,.38,seed);
          bloom=max(bloom,silhouette*presence*(.32+.68*feather)*rings);
          facets=max(facets,exp(-abs(edge)*100.0)*presence*.65);
        }
        return vec2(bloom,facets);
      }
      void main() {
        vec2 disk=v_uv*2.0-1.0;
        float radius=length(disk);
        float alpha=1.0-smoothstep(.985-2.0/u_resolution.x,.985,radius);
        if(alpha<=0.0){gl_FragColor=vec4(0);return;}
        float flex=1.0-smoothstep(.48,.985,radius);
        vec2 p=disk;
        vec2 local=p-vec2(-.36,.12);
        p+=vec2(-local.y,local.x)*u_flow.z*flex*exp(-dot(local,local)*2.2);
        local=p-vec2(.43,-.22);
        p+=vec2(local.y,-local.x)*u_flow.w*flex*exp(-dot(local,local)*2.6);
        vec2 advected=p+u_flow.xy*.66;
        vec2 q=advected*2.0;
        vec2 curl=vec2(fbm(q+vec2(u_time*.035,0)),fbm(q+vec2(8.3,-u_time*.029)))-.47;
        q+=curl*(1.2+u_energy*.38);
        float pool=fbm(q*1.45+4.7);
        float vein=fbm(q*2.7+vec2(0,u_time*.018));
        vec3 color=mix(u_forest,u_green,smoothstep(.18,.68,pool));
        float tide=smoothstep(.41,.66,vein+.12*pool);
        color=mix(color,u_mist,tide*.40);
        // Slow rivulets between pools, varied in both width and direction.
        float stream=1.0-smoothstep(.025,.11,abs(pool+.13*vein-.49));
        color=mix(color,mix(u_green,u_mist,.6),stream*.26);
        vec2 mineral=crystals(advected*6.5+curl*1.4);
        float growth=smoothstep(.27,.64,fbm(q*2.0+16.0));
        color=mix(color,u_mist,mineral.x*(.28+growth*.60));
        color=mix(color,u_air,mineral.y*growth*.44);
        // Dense pin-sized deposits, clustered rather than screen-space noise.
        float speck=pow(noise(q*84.0),12.0);
        color=mix(color,u_air,min(.65,speck*(1.3+growth*3.0)));
        // A thin, irregular fired edge; no spherical lighting / 3D highlight.
        float rim=smoothstep(.91,.985,radius+noise(disk*36.0)*.014);
        color=mix(color,mix(u_air,u_green,.42),rim*.63);
        color*=.96+.07*noise(q*105.0);
        gl_FragColor=vec4(color*alpha,alpha);
      }
    `,
  };
})();
